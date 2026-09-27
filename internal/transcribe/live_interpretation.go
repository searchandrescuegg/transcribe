package transcribe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/searchandrescuegg/transcribe/internal/ml"
	"github.com/slack-go/slack"
)

// Live-interpretation feature: every TAC transmission appends a transcript entry to the
// per-TAC list, then re-summarizes the rescue (dispatch + all transcripts so far) and
// updates a "Live Interpretation" message in the rescue thread. Each summary is anchored
// in cumulative context, so as the rescue expands the model sees the full history.
//
// Storage:
//   LIST   tac_transcripts:<TGID>  → JSON-encoded {captured_at, text} per RPush
//   STRING summary_ts:<TGID>       → message_ts of the running interpretation message
//
// Both keys carry a TTL of 2 × TacticalChannelActivationDuration so a Switch that resets
// the activation window doesn't lose mid-rescue context. The sweeper's metadata cleanup
// also DELs them on closure.

const (
	tacTranscriptsKeyFmt = "tac_transcripts:%s"
	summaryTSKeyFmt      = "summary_ts:%s"
	// summaryDataKeyFmt holds the latest JSON-encoded RescueSummary so the sweeper /
	// feedback path can read fields (headline, situation summary) at close time without
	// re-running the LLM. Updated on every successful summarize pass; deleted alongside
	// the other live-interpretation sidecars on closure.
	summaryDataKeyFmt = "summary_data:%s"
	// summaryLockKeyFmt protects against the burst-of-concurrent-transmissions case where
	// multiple workers all try to SummarizeRescue at the same time. The cluster llama-cpp
	// serializes generation server-side, so concurrent calls just queue and the later ones
	// time out — wasted work and noisy logs. With a SetNX lock, only the holder runs the
	// LLM call; everyone else marks the rescue stale and returns instantly.
	summaryLockKeyFmt = "summary_lock:%s"
	// summaryStaleKeyFmt is the "another transmission arrived during the last LLM call"
	// flag the lock holder checks before releasing. Lets one worker batch up an arbitrary
	// number of concurrent transmissions into exactly two LLM calls (initial + catch-up).
	summaryStaleKeyFmt = "summary_stale:%s"

	// summaryLockTTL must outlast the slowest expected ML round-trip + post — anything
	// shorter risks a second worker grabbing the lock while the holder is still working.
	// We tie this to OPENAI_TIMEOUT-equivalent semantics: the holder either finishes within
	// this window or its lock expires and another worker can pick up.
	summaryLockTTL = 150 * time.Second
	// summaryStaleTTL is short on purpose: the flag is meaningful only for the brief window
	// between RPush-during-lock and the lock holder's next stale-check. Beyond that the
	// next transmission will retrigger the path naturally.
	summaryStaleTTL = 60 * time.Second

	// summary_stale values. "1" = a transmission arrived during the last pass, rerun additively.
	// "rewrite" = an edit/retraction arrived, rerun WITHOUT the previous summary. A pending
	// "rewrite" is never downgraded: plain losers use SETNX, rewrite losers use SET.
	staleValueRerun   = "1"
	staleValueRewrite = "rewrite"
)

// updateLiveInterpretation appends one radio transcript and refreshes the running summary.
// Kept for callers/tests that only have text; processNonDispatchCall uses appendAndRefresh
// directly so it can record the Slack post ts and S3 key.
func (tc *TranscribeClient) updateLiveInterpretation(ctx context.Context, tacTGID string, capturedAt time.Time, transcript string) {
	tc.appendAndRefresh(ctx, tacTGID, liveTranscriptEntry{
		CapturedAt: capturedAt.Format("15:04:05"),
		Text:       transcript,
		Kind:       entryKindRadio,
	})
}

// appendAndRefresh records one entry (lossless — every worker appends) then runs an additive
// summary refresh. Best-effort: failures log and return.
func (tc *TranscribeClient) appendAndRefresh(ctx context.Context, tgid string, e liveTranscriptEntry) {
	if e.Text == "" {
		return
	}
	if err := tc.appendEntry(ctx, tgid, e); err != nil {
		slog.Warn("live interpretation: append failed", slog.String("error", err.Error()), slog.String("tgid", tgid))
		return
	}
	tc.RefreshLiveInterpretation(ctx, tgid, false)
}

// RefreshLiveInterpretation re-summarizes the rescue and posts/updates the Live Interpretation.
// rewrite=true runs the first pass without the previous summary (used after a human edit or
// retraction so facts derived from superseded text are re-derived).
//
// Concurrency model (unchanged from the original burst design): a per-TGID SETNX lock admits one
// holder; losers leave a signal in summary_stale and return. The holder loops, consuming the
// signal with GETDEL before each pass so a "rewrite" that lands mid-pass is never lost.
func (tc *TranscribeClient) RefreshLiveInterpretation(ctx context.Context, tgid string, rewrite bool) {
	lockKey := fmt.Sprintf(summaryLockKeyFmt, tgid)
	staleKey := fmt.Sprintf(summaryStaleKeyFmt, tgid)

	acquired, err := tc.dragonflyClient.SetNX(ctx, lockKey, summaryLockTTL, "1")
	if err != nil {
		slog.Warn("live interpretation: lock SetNX failed; skipping summary update", slog.String("error", err.Error()), slog.String("tgid", tgid))
		return
	}
	if !acquired {
		tc.markSummaryStale(ctx, staleKey, rewrite, tgid)
		return
	}
	defer func() {
		if err := tc.dragonflyClient.Del(ctx, lockKey); err != nil {
			slog.Warn("live interpretation: failed to release summary lock; will expire via TTL", slog.String("error", err.Error()), slog.String("tgid", tgid))
		}
	}()

	const maxIterations = 5
	for i := 0; i < maxIterations; i++ {
		pending, err := tc.dragonflyClient.GetDel(ctx, staleKey)
		if err != nil {
			slog.Warn("live interpretation: failed to consume stale flag", slog.String("error", err.Error()))
		}
		if pending == staleValueRewrite {
			rewrite = true
		}
		if !tc.runOneSummaryPass(ctx, tgid, rewrite) {
			return
		}
		next, err := tc.dragonflyClient.Get(ctx, staleKey)
		if err != nil {
			slog.Warn("live interpretation: failed to read stale flag; assuming caught up", slog.String("error", err.Error()))
			return
		}
		if next == "" {
			return
		}
		rewrite = false // the next iteration's GETDEL re-derives it from the pending value
		slog.Debug("live interpretation: stale flag set during summary; re-running", slog.String("tgid", tgid), slog.Int("iteration", i+1))
	}
	slog.Warn("live interpretation: hit maxIterations; giving up to avoid infinite loop", slog.String("tgid", tgid))
}

func (tc *TranscribeClient) markSummaryStale(ctx context.Context, staleKey string, rewrite bool, tgid string) {
	var err error
	if rewrite {
		err = tc.dragonflyClient.Set(ctx, staleKey, summaryStaleTTL, staleValueRewrite)
	} else {
		// SETNX so a pending "rewrite" is never downgraded to a plain rerun.
		_, err = tc.dragonflyClient.SetNX(ctx, staleKey, summaryStaleTTL, staleValueRerun)
	}
	if err != nil {
		slog.Warn("live interpretation: failed to set stale flag", slog.String("error", err.Error()), slog.String("tgid", tgid))
	}
}

// runOneSummaryPass reads the full transcripts list, calls SummarizeRescue, and posts (or
// chat.update's) the running interpretation message. Returns false on terminal failures
// (no metadata, ML unrecoverable error) so the caller stops iterating.
func (tc *TranscribeClient) runOneSummaryPass(ctx context.Context, tacTGID string, rewrite bool) bool {
	entries, err := tc.readEntries(ctx, tacTGID)
	if err != nil {
		slog.Warn("live interpretation: LRange failed", slog.String("error", err.Error()), slog.String("tgid", tacTGID))
		return false
	}

	meta, ok := tc.readClosureMeta(ctx, tacTGID)
	if !ok {
		return false
	}

	// Additive context (see rule 12) unless this pass is a rewrite after a human edit/retraction.
	var previousSummary *ml.RescueSummary
	if !rewrite {
		previousSummary, _ = tc.readSummaryData(ctx, tacTGID)
	}
	unitContext := tc.unitContextFor(ctx, tacTGID, meta.Transcription, time.Now())
	input := buildSummaryInput(meta, entries, previousSummary, unitContext)

	summary, err := tc.mlClient.SummarizeRescue(ctx, input)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			slog.Warn("live interpretation: shutdown interrupted summarize", slog.String("error", err.Error()))
			return false
		}
		slog.Error("live interpretation: SummarizeRescue failed", slog.String("error", err.Error()), slog.String("tgid", tacTGID))
		return false
	}

	tc.publishLiveInterpretation(ctx, tacTGID, meta, summary, tc.transcriptsTTL())
	slog.Info("live interpretation: posted summary",
		slog.String("tgid", tacTGID),
		slog.Int("transcripts_count", len(input.TACTranscripts)), slog.Int("operator_corrections", len(input.OperatorCorrections)), slog.Bool("rewrite", rewrite),
		slog.String("headline", summary.Headline))
	return true
}

// buildSummaryInput turns the stored entries into the summarizer input: radio entries (with
// human corrections applied and flagged) become TACTranscripts, live operator entries become
// OperatorCorrections, and invalid/tombstoned entries are skipped.
func buildSummaryInput(meta ClosureMeta, entries []liveTranscriptEntry, previous *ml.RescueSummary, unitContext string) ml.RescueSummaryInput {
	in := ml.RescueSummaryInput{
		DispatchTranscription: meta.Transcription,
		DispatchCallType:      "Rescue - Trail",
		TACChannel:            meta.TACChannel,
		TACTranscripts:        make([]ml.TACTranscript, 0, len(entries)),
		PreviousSummary:       previous,
		UnitContext:           unitContext,
		DispatchVerified:      meta.DispatchCorrection != nil,
	}
	for _, e := range entries {
		switch e.kind() {
		case entryKindRadio:
			in.TACTranscripts = append(in.TACTranscripts, ml.TACTranscript{
				CapturedAt: e.CapturedAt, Text: e.effectiveText(), Verified: e.Correction != nil,
			})
		case entryKindOperator:
			if e.Deleted || strings.TrimSpace(e.Text) == "" {
				continue
			}
			in.OperatorCorrections = append(in.OperatorCorrections, ml.OperatorCorrection{At: e.CapturedAt, Text: e.Text})
		}
	}
	return in
}

// publishLiveInterpretation posts (or chat.updates) the running-summary message in the
// rescue thread. The message_ts is cached in summary_ts:<TGID> with the same TTL as the
// transcripts list so an active rescue keeps a stable summary anchor.
func (tc *TranscribeClient) publishLiveInterpretation(ctx context.Context, tacTGID string, meta ClosureMeta, summary *ml.RescueSummary, ttl time.Duration) {
	// Read the previous SAR-notified state BEFORE overwriting summary_data, so we can detect
	// the false→true transition and badge the parent alert exactly once (see below).
	wasNotified := tc.summarySARNotified(ctx, tacTGID)

	// Cache the latest structured summary so the close path can prefill the feedback form
	// without needing to re-run the LLM. Best-effort — if this write fails the live message
	// still posts; the feedback button will just open with fewer prefilled fields.
	if encoded, err := json.Marshal(summary); err == nil {
		if err := tc.dragonflyClient.Set(ctx, fmt.Sprintf(summaryDataKeyFmt, tacTGID), ttl, string(encoded)); err != nil {
			slog.Warn("live interpretation: failed to cache summary_data; feedback prefill may be incomplete",
				slog.String("error", err.Error()),
				slog.String("tgid", tacTGID))
		}
	}

	// On the first transmission that reports SAR notification, badge the parent alert with the
	// green check. Gated on the false→true transition (via the pre-write read above) so we do
	// exactly one extra chat.update per rescue, not one per subsequent transmission. SAR
	// notification is monotonic — the mention stays in the cumulative transcript history — so
	// once badged it stays badged.
	if summary.SARNotified && !wasNotified {
		tc.badgeParentAlertSAR(ctx, tacTGID, meta)
	}

	blocks := BuildLiveInterpretationBlocks(summary, time.Now().Local())
	fallback := summary.Headline
	if fallback == "" {
		fallback = "Live interpretation updated"
	}

	tsKey := fmt.Sprintf(summaryTSKeyFmt, tacTGID)
	existingTS, err := tc.dragonflyClient.Get(ctx, tsKey)
	if err != nil {
		slog.Warn("live interpretation: failed to read summary_ts; will post a new message", slog.String("error", err.Error()))
		existingTS = ""
	}

	if existingTS != "" {
		// Update the existing message in-place.
		if _, _, _, err := tc.slackClient.UpdateMessageContext(ctx,
			tc.config.SlackChannelID,
			existingTS,
			slack.MsgOptionBlocks(blocks...),
			slack.MsgOptionText(fallback, false),
		); err != nil {
			slog.Warn("live interpretation: chat.update failed; thread message will be stale until next transmission",
				slog.String("error", err.Error()),
				slog.String("tgid", tacTGID))
		}
		return
	}

	// First TAC transmission for this rescue — post a new threaded message and remember
	// its ts so subsequent transmissions update it.
	_, postedTS, err := tc.sendSlackInThread(ctx, meta.SourceTalkgroup, meta.ThreadTS, blocks, fallback)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			slog.Warn("live interpretation: shutdown interrupted thread post", slog.String("error", err.Error()))
			return
		}
		slog.Error("live interpretation: thread post failed", slog.String("error", err.Error()), slog.String("tgid", tacTGID))
		return
	}
	if err := tc.dragonflyClient.Set(ctx, tsKey, ttl, postedTS); err != nil {
		slog.Warn("live interpretation: failed to persist summary_ts; next update will post a duplicate",
			slog.String("error", err.Error()),
			slog.String("tgid", tacTGID))
	}
}

// badgeParentAlertSAR re-renders the parent rescue alert with the green-check "Search &
// Rescue notified" badge and chat.update's it in place. Called once per rescue on the
// SAR-notified transition. Best-effort: any failure logs and returns — the live
// interpretation message still carries the SAR badge regardless.
//
// The current expiry is read from the active_tacs ZSET (score = unix expiry) so the live
// "Expires …" line stays accurate — including after an Extend — without threading the expiry
// through ClosureMeta. If the expiry can't be read (rescue already closing/closed), skip
// rather than render a bogus timestamp.
func (tc *TranscribeClient) badgeParentAlertSAR(ctx context.Context, tgid string, meta ClosureMeta) {
	if meta.MessageTS == "" || meta.Transcription == "" {
		return // no message to update, or can't rebuild the alert faithfully
	}

	score, err := tc.dragonflyClient.ZScore(ctx, activeTACsKey, tgid)
	if err != nil {
		slog.Warn("live interpretation: SAR badge skipped; could not read expiry from active_tacs",
			slog.String("error", err.Error()), slog.String("tgid", tgid))
		return
	}
	expiresAt := time.Unix(int64(score), 0).Local()

	blocks := BuildRescueTrailBlocks(&RescueTrailBlocksInput{
		TACChannel:        meta.TACChannel,
		TranscriptionText: meta.Transcription,
		ExpiresAt:         expiresAt,
		DispatchTGID:      FireDispatch1TGID,
		TACTalkgroupTGID:  meta.TGID, // keeps the Cancel/Close/Extend/Switch actions on the live alert
		SARNotified:       true,
	})

	updateCtx, cancel := context.WithTimeout(ctx, tc.config.SlackTimeout)
	defer cancel()
	if _, _, _, err := tc.slackClient.UpdateMessageContext(updateCtx,
		tc.config.SlackChannelID,
		meta.MessageTS,
		slack.MsgOptionBlocks(blocks...),
		slack.MsgOptionText(fmt.Sprintf("%s — Search & Rescue notified", meta.TACChannel), false),
	); err != nil {
		slog.Warn("live interpretation: failed to badge parent alert with SAR-notified",
			slog.String("error", err.Error()), slog.String("tgid", tgid), slog.String("message_ts", meta.MessageTS))
		return
	}
	slog.Info("live interpretation: badged parent alert — SAR notified",
		slog.String("tgid", tgid), slog.String("tac", meta.TACChannel))
}

// sendSlackInThread is a convenience wrapper that goes through sendSlackWithRetry and also
// returns the ts of the posted message (Slack's chat.postMessage returns it in the second
// return slot; sendSlackWithRetry returns only the ts so we can carry it forward).
func (tc *TranscribeClient) sendSlackInThread(ctx context.Context, talkgroup, threadTS string, blocks []slack.Block, fallback string) (string, string, error) {
	ts, err := tc.sendSlackWithRetry(ctx, talkgroup,
		slack.MsgOptionBlocks(blocks...),
		slack.MsgOptionTS(threadTS),
		slack.MsgOptionAsUser(true),
		slack.MsgOptionText(fallback, false),
	)
	return tc.config.SlackChannelID, ts, err
}

// readClosureMeta reads tac_meta:<TGID> and JSON-decodes it. Returns ok=false (no error)
// when the metadata is missing — the rescue has likely been cancelled or auto-expired and
// any further work is a no-op.
func (tc *TranscribeClient) readClosureMeta(ctx context.Context, tgid string) (ClosureMeta, bool) {
	raw, err := tc.dragonflyClient.Get(ctx, fmt.Sprintf(tacMetaKeyFmt, tgid))
	if err != nil {
		slog.Warn("live interpretation: failed to read tac_meta", slog.String("error", err.Error()), slog.String("tgid", tgid))
		return ClosureMeta{}, false
	}
	if raw == "" {
		return ClosureMeta{}, false
	}
	var meta ClosureMeta
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		slog.Warn("live interpretation: tac_meta unparseable", slog.String("error", err.Error()), slog.String("tgid", tgid))
		return ClosureMeta{}, false
	}
	return meta, true
}
