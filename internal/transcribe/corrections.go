package transcribe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/searchandrescuegg/transcribe/internal/dataset"
	"github.com/searchandrescuegg/transcribe/internal/ml"
	"github.com/slack-go/slack"
)

// rerenderParentAlert rebuilds the live (not closed) rescue alert from the latest tac_meta plus
// summary_data — transcription, dispatch-correction label, brief, SAR badge, status line, action
// buttons — and chat.updates it. It is the single live parent-alert render path: it derives the
// SAR badge and brief itself so no caller (summary pass, dispatch correction) can drop another's
// decoration. Best-effort: returns false on any failure or skip.
//
// Skips (no chat.update) when the rescue is closing or closed: the expiry in active_tacs can't
// be read, or it is not in the future (Close sets the score to now-1 until the sweeper's next
// tick). A live render then would put live buttons and a past expiry over the closed alert.
//
// Rebuild-on-retry: a retryable rate limit waits RetryAfter and then RE-RENDERS from current
// state rather than resending the blocks built before the wait, so a rescue that closed (or a
// brief / dispatch correction that changed) during the wait is never overwritten with stale
// content. meta is only a fallback when tac_meta can't be read. An empty fallback text is
// derived from the freshly-read state.
func (tc *TranscribeClient) rerenderParentAlert(ctx context.Context, meta ClosureMeta, fallback string) bool {
	if meta.MessageTS == "" || meta.Transcription == "" {
		return false
	}
	// render returns attempted=false when it skipped without calling Slack.
	render := func() (attempted bool, err error) {
		score, err := tc.dragonflyClient.ZScore(ctx, activeTACsKey, meta.TGID)
		if err != nil {
			slog.Warn("parent alert re-render skipped; could not read expiry from active_tacs",
				slog.String("error", err.Error()), slog.String("tgid", meta.TGID))
			return false, nil
		}
		expiresAt := time.Unix(int64(score), 0)
		if !expiresAt.After(time.Now()) {
			slog.Debug("parent alert re-render skipped; rescue is closing",
				slog.String("tgid", meta.TGID), slog.Time("expires_at", expiresAt))
			return false, nil
		}
		m := tc.freshClosureMeta(ctx, meta.TGID, meta)
		if m.MessageTS == "" || m.Transcription == "" {
			return false, nil
		}
		summary, _ := tc.readSummaryData(ctx, meta.TGID)
		text := fallback
		if text == "" {
			text = parentAlertFallback(m, summary)
		}
		blocks := BuildRescueTrailBlocks(&RescueTrailBlocksInput{
			TACChannel:        m.TACChannel,
			TranscriptionText: m.Transcription,
			ExpiresAt:         expiresAt.Local(),
			DispatchTGID:      FireDispatch1TGID,
			TACTalkgroupTGID:  m.TGID,
			SARNotified:       summary != nil && summary.SARNotified,
			Correction:        m.DispatchCorrection,
			Brief:             FormatBrief(summary),
		})
		uctx, cancel := context.WithTimeout(ctx, tc.config.SlackTimeout)
		defer cancel()
		_, _, _, err = tc.slackClient.UpdateMessageContext(uctx, tc.config.SlackChannelID, m.MessageTS,
			slack.MsgOptionBlocks(blocks...), slack.MsgOptionText(text, false))
		return true, err
	}

	attempted, err := render()
	var rate *slack.RateLimitedError
	if errors.As(err, &rate) && rate.Retryable() {
		select {
		case <-ctx.Done():
			err = ctx.Err()
		case <-time.After(rate.RetryAfter):
			attempted, err = render()
		}
	}
	if err != nil {
		slog.Warn("parent alert re-render failed",
			slog.String("error", err.Error()), slog.String("tgid", meta.TGID), slog.String("message_ts", meta.MessageTS))
		return false
	}
	return attempted
}

// parentAlertFallback is the notification/fallback text for a parent-alert re-render: the brief
// when there is one, else the SAR badge text, else a generic "updated".
func parentAlertFallback(meta ClosureMeta, s *ml.RescueSummary) string {
	if s != nil {
		if brief := FormatBrief(s); brief != "" {
			return fmt.Sprintf("%s — %s", meta.TACChannel, brief)
		}
		if s.SARNotified {
			return fmt.Sprintf("%s — Search & Rescue notified", meta.TACChannel)
		}
	}
	return fmt.Sprintf("%s — rescue alert updated", meta.TACChannel)
}

// correctedGuardKeyFmt is the one-shot guard for the "Correct transcript" shortcut, keyed by the
// corrected Slack message ts. SETNX makes concurrent submissions race-safe. TTL-only by design:
// a Slack ts is unique, so it can never leak into a reopened rescue (no invariant-#6 cleanup).
const correctedGuardKeyFmt = "corrected:%s"

type CorrectionKind string

const (
	CorrectionKindDispatch CorrectionKind = "dispatch"
	CorrectionKindTAC      CorrectionKind = "tac"
)

// CorrectionTarget identifies one correctable Slack post and the text it currently shows.
type CorrectionTarget struct {
	TGID        string
	Kind        CorrectionKind
	MessageTS   string
	ThreadTS    string
	CurrentText string
}

var (
	ErrRescueNotActive     = errors.New("rescue is not active")
	ErrNotCorrectable      = errors.New("message is not a correctable transcription")
	ErrEmptyCorrection     = errors.New("correction text is empty")
	ErrUnchangedCorrection = errors.New("correction text is unchanged")
)

// AlreadyCorrectedError reports the existing correction so the UI can say who and when.
type AlreadyCorrectedError struct{ Correction TranscriptCorrection }

func (e *AlreadyCorrectedError) Error() string {
	return fmt.Sprintf("already corrected by %s at %s", e.Correction.By, e.Correction.At.Format(time.RFC3339))
}

// ValidateCorrectionText trims the proposal and rejects empty or unchanged text.
func ValidateCorrectionText(current, proposed string) (string, error) {
	text := strings.TrimSpace(proposed)
	if text == "" {
		return "", ErrEmptyCorrection
	}
	if text == strings.TrimSpace(current) {
		return "", ErrUnchangedCorrection
	}
	return text, nil
}

// LookupTGIDByThread finds the active rescue whose alert thread is threadTS by scanning
// active_tacs (a handful of members) and matching tac_meta.ThreadTS. No reverse-index key.
func (tc *TranscribeClient) LookupTGIDByThread(ctx context.Context, threadTS string) (string, ClosureMeta, bool, error) {
	if threadTS == "" {
		return "", ClosureMeta{}, false, nil
	}
	tgids, err := tc.dragonflyClient.ZRangeByScore(ctx, activeTACsKey, "-inf", "+inf")
	if err != nil {
		return "", ClosureMeta{}, false, fmt.Errorf("scan active_tacs: %w", err)
	}
	for _, tgid := range tgids {
		if meta, ok := tc.readClosureMeta(ctx, tgid); ok && meta.ThreadTS == threadTS {
			return tgid, meta, true, nil
		}
	}
	return "", ClosureMeta{}, false, nil
}

// ResolveCorrectionTarget classifies a Slack message as the dispatch alert or a TAC
// transmission post of an active rescue. threadTS may be empty for the parent alert.
func (tc *TranscribeClient) ResolveCorrectionTarget(ctx context.Context, messageTS, threadTS string) (CorrectionTarget, error) {
	if threadTS == "" {
		threadTS = messageTS
	}
	tgid, meta, ok, err := tc.LookupTGIDByThread(ctx, threadTS)
	if err != nil {
		return CorrectionTarget{}, err
	}
	if !ok {
		return CorrectionTarget{}, ErrRescueNotActive
	}
	if messageTS == meta.MessageTS {
		if meta.DispatchCorrection != nil {
			return CorrectionTarget{}, &AlreadyCorrectedError{Correction: *meta.DispatchCorrection}
		}
		return CorrectionTarget{TGID: tgid, Kind: CorrectionKindDispatch, MessageTS: messageTS, ThreadTS: meta.ThreadTS, CurrentText: meta.Transcription}, nil
	}
	_, e, found, err := tc.findEntryBySlackTS(ctx, tgid, messageTS, entryKindRadio)
	if err != nil {
		return CorrectionTarget{}, err
	}
	if !found {
		return CorrectionTarget{}, ErrNotCorrectable
	}
	if e.Correction != nil {
		return CorrectionTarget{}, &AlreadyCorrectedError{Correction: *e.Correction}
	}
	return CorrectionTarget{TGID: tgid, Kind: CorrectionKindTAC, MessageTS: messageTS, ThreadTS: meta.ThreadTS, CurrentText: e.Text}, nil
}

// ApplyTranscriptCorrection stores a one-shot human correction and relabels the Slack post.
// Order: validate → SETNX guard → mutate state → chat.update label → dataset. The guard is
// released if the state mutation fails so the message isn't permanently locked. The caller
// runs RefreshLiveInterpretation(ctx, target.TGID, true) afterwards.
func (tc *TranscribeClient) ApplyTranscriptCorrection(ctx context.Context, target CorrectionTarget, userID, newText string, now time.Time) error {
	text, err := ValidateCorrectionText(target.CurrentText, newText)
	if err != nil {
		return err
	}
	corr := TranscriptCorrection{By: userID, At: now}
	guardKey := fmt.Sprintf(correctedGuardKeyFmt, target.MessageTS)
	guardVal, _ := json.Marshal(corr)
	acquired, err := tc.dragonflyClient.SetNX(ctx, guardKey, tc.transcriptsTTL(), string(guardVal))
	if err != nil {
		return fmt.Errorf("correction guard: %w", err)
	}
	if !acquired {
		var existing TranscriptCorrection
		raw, _ := tc.dragonflyClient.Get(ctx, guardKey)
		_ = json.Unmarshal([]byte(raw), &existing)
		return &AlreadyCorrectedError{Correction: existing}
	}

	var rec dataset.HumanCorrectionRecord
	switch target.Kind {
	case CorrectionKindTAC:
		rec, err = tc.applyTACCorrection(ctx, target, text, corr)
	case CorrectionKindDispatch:
		rec, err = tc.applyDispatchCorrection(ctx, target, text, corr)
	default:
		err = ErrNotCorrectable
	}
	if err != nil {
		if delErr := tc.dragonflyClient.Del(ctx, guardKey); delErr != nil {
			slog.Warn("corrections: failed to release guard", slog.String("error", delErr.Error()), slog.String("message_ts", target.MessageTS))
		}
		return err
	}
	rec.Action = dataset.HumanCorrectionActionCreate
	rec.TGID, rec.SlackTS, rec.SlackUserID, rec.CorrectedText = target.TGID, target.MessageTS, userID, text
	tc.recordHumanCorrection(rec)
	slog.Info("corrections: transcript corrected",
		slog.String("tgid", target.TGID), slog.String("kind", string(target.Kind)), slog.String("user", userID))
	return nil
}

func (tc *TranscribeClient) applyTACCorrection(ctx context.Context, target CorrectionTarget, text string, corr TranscriptCorrection) (dataset.HumanCorrectionRecord, error) {
	idx, e, found, err := tc.findEntryBySlackTS(ctx, target.TGID, target.MessageTS, entryKindRadio)
	if err != nil {
		return dataset.HumanCorrectionRecord{}, err
	}
	if !found {
		return dataset.HumanCorrectionRecord{}, ErrNotCorrectable
	}
	if e.Correction != nil {
		return dataset.HumanCorrectionRecord{}, &AlreadyCorrectedError{Correction: *e.Correction}
	}
	corr.Original = e.Text
	e.Corrected, e.Correction = text, &corr
	if err := tc.setEntry(ctx, target.TGID, idx, e); err != nil {
		return dataset.HumanCorrectionRecord{}, err
	}

	postedAt, perr := time.Parse(time.RFC3339, e.PostedAt)
	if perr != nil {
		postedAt = corr.At // legacy entry without posted_at: label still renders
	}
	channelName := target.TGID
	if tg, ok := talkgroupFromTGID[target.TGID]; ok {
		channelName = tg.FullName
	}
	blocks := BuildThreadCommunicationBlocks(&ThreadCommunicationBlocksInput{
		Channel: channelName, Message: text, TS: postedAt.Local(), Correction: &corr,
	})
	if err := tc.updateMessageWithRetry(ctx, target.MessageTS, slack.MsgOptionBlocks(blocks...), slack.MsgOptionText(text, false)); err != nil {
		slog.Warn("corrections: failed to relabel corrected post", slog.String("error", err.Error()), slog.String("message_ts", target.MessageTS))
	}

	return dataset.HumanCorrectionRecord{Kind: dataset.HumanCorrectionKindTAC, S3Key: e.S3Key, PriorText: e.Text}, nil
}

func (tc *TranscribeClient) applyDispatchCorrection(ctx context.Context, target CorrectionTarget, text string, corr TranscriptCorrection) (dataset.HumanCorrectionRecord, error) {
	meta, ok := tc.readClosureMeta(ctx, target.TGID)
	if !ok {
		return dataset.HumanCorrectionRecord{}, ErrRescueNotActive
	}
	if meta.DispatchCorrection != nil {
		return dataset.HumanCorrectionRecord{}, &AlreadyCorrectedError{Correction: *meta.DispatchCorrection}
	}
	corr.Original = meta.Transcription
	meta.Transcription, meta.DispatchCorrection = text, &corr
	payload, err := json.Marshal(meta)
	if err != nil {
		return dataset.HumanCorrectionRecord{}, fmt.Errorf("marshal closure meta: %w", err)
	}
	// SetXX (not Set): tac_meta may have been deleted between our read and this write (a
	// sweeper close, Cancel, or Switch racing this correction). A plain Set would resurrect
	// the key with a fresh 24h TTL and none of active_tacs / allowed_talkgroups / tg:<TGID>
	// behind it, silently reviving a dead rescue for a full day. If it's gone, the rescue is
	// no longer active — surface that instead of writing a zombie key.
	set, err := tc.dragonflyClient.SetXX(ctx, fmt.Sprintf(tacMetaKeyFmt, meta.TGID), string(payload))
	if err != nil {
		return dataset.HumanCorrectionRecord{}, fmt.Errorf("write closure meta: %w", err)
	}
	if !set {
		return dataset.HumanCorrectionRecord{}, ErrRescueNotActive
	}
	tc.rerenderParentAlert(ctx, meta, fmt.Sprintf("%s — dispatch transcript corrected", meta.TACChannel))
	return dataset.HumanCorrectionRecord{Kind: dataset.HumanCorrectionKindDispatch, S3Key: meta.DispatchS3Key, PriorText: corr.Original}, nil
}

// updateMessageWithRetry chat.updates a post, retrying once on a retryable rate limit. Each
// attempt is bounded by SlackTimeout. Used by the TAC-post relabel path; it resends the same
// blocks, so it must NOT be used for the parent alert (rerenderParentAlert rebuilds on retry
// instead, because that alert's content can change during the wait). Best-effort: the stored correction
// is authoritative; the label is presentation — callers log the returned error and move on.
func (tc *TranscribeClient) updateMessageWithRetry(ctx context.Context, ts string, opts ...slack.MsgOption) error {
	update := func() error {
		uctx, cancel := context.WithTimeout(ctx, tc.config.SlackTimeout)
		defer cancel()
		_, _, _, err := tc.slackClient.UpdateMessageContext(uctx, tc.config.SlackChannelID, ts, opts...)
		return err
	}
	err := update()
	var rate *slack.RateLimitedError
	if errors.As(err, &rate) && rate.Retryable() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(rate.RetryAfter):
		}
		err = update()
	}
	return err
}

func (tc *TranscribeClient) recordHumanCorrection(rec dataset.HumanCorrectionRecord) {
	if tc.recorder != nil {
		tc.recorder.RecordHumanCorrection(rec)
	}
}
