package transcribe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/slack-go/slack"
)

// FIX (review item #10 / option B): durable replacement for the in-process time.AfterFunc
// that previously scheduled "channel closed" Slack messages. Pending closures are persisted
// in a Dragonfly ZSET (score=unix-expiry) and a sweeper goroutine claims-and-posts due
// entries via ZRem. Restarts no longer drop the closing message.
//
// Data model (also used by the Slack interactivity controller in internal/slackctl):
//   ZSET active_tacs            : member = TGID,           score = unix expiry timestamp
//   STRING tac_meta:<TGID>       : JSON-encoded ClosureMeta (TAC channel, thread_ts, etc.)
//
// TGID-as-member lets the cancel handler ZRem the pending closure in O(1) without scanning
// the whole set. Metadata lives in a sibling key so the ZSET stays small (sweeper queries
// it on every tick).

const (
	activeTACsKey  = "active_tacs"
	tacMetaKeyFmt  = "tac_meta:%s"
	closureMetaTTL = 24 * time.Hour // safety net so orphaned metadata can't accumulate forever
)

// ClosureMeta is exported so the slackctl controller can deserialize it without duplicating
// the schema. Field tags must match the marshalled form on disk.
type ClosureMeta struct {
	TGID            string `json:"tgid"`
	TACChannel      string `json:"tac_channel"`
	ThreadTS        string `json:"thread_ts"`
	SourceTalkgroup string `json:"source_talkgroup"`
	// MessageTS is the ts of the original alert message (== thread parent). Stored so the
	// controller can chat.update the alert when leadership cancels or extends.
	MessageTS string `json:"message_ts"`
	// Transcription is the original dispatch transcript that produced this rescue alert.
	// Stored so the sweeper can rebuild the alert blocks (preserving the transcript)
	// when the auto-close fires and we need to remove the action buttons.
	Transcription string `json:"transcription,omitempty"`
}

// ScheduleTACClosure persists a pending channel-closed notification keyed by expiry time.
// Replaces the previous in-memory time.AfterFunc which did not survive process restarts.
// Exported so the Slack interactivity controller can call it on Extend.
func (tc *TranscribeClient) ScheduleTACClosure(ctx context.Context, meta ClosureMeta, expiresAt time.Time) error {
	if meta.TGID == "" {
		return errors.New("ScheduleTACClosure: TGID is required")
	}
	payload, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal closure meta: %w", err)
	}
	if err := tc.dragonflyClient.Set(ctx, fmt.Sprintf(tacMetaKeyFmt, meta.TGID), closureMetaTTL, string(payload)); err != nil {
		return fmt.Errorf("write closure metadata: %w", err)
	}
	if err := tc.dragonflyClient.ZAdd(ctx, activeTACsKey, float64(expiresAt.Unix()), meta.TGID); err != nil {
		return fmt.Errorf("schedule closure in ZSET: %w", err)
	}
	return nil
}

// Sweep is the long-running loop that polls for due closures and posts them.
// Intended to be run as a goroutine alongside the worker pool.
func (tc *TranscribeClient) Sweep(ctx context.Context) {
	ticker := time.NewTicker(tc.config.TACSweeperInterval)
	defer ticker.Stop()
	slog.Info("TAC closure sweeper started", slog.Duration("interval", tc.config.TACSweeperInterval))
	for {
		select {
		case <-ctx.Done():
			slog.Info("TAC closure sweeper stopping")
			return
		case <-ticker.C:
			tc.sweepOnce(ctx)
		}
	}
}

func (tc *TranscribeClient) sweepOnce(ctx context.Context) {
	now := strconv.FormatInt(time.Now().Unix(), 10)
	tgids, err := tc.dragonflyClient.ZRangeByScore(ctx, activeTACsKey, "0", now)
	if err != nil {
		slog.Error("sweeper: failed to query active_tacs", slog.String("error", err.Error()))
		return
	}
	for _, tgid := range tgids {
		// FIX (concurrency): ZRem returns 1 only for the goroutine that actually removed the
		// member, so even with multiple sweeper instances each due closure is delivered exactly
		// once. We claim before reading metadata so a competing cancel-handler can't race us.
		removed, err := tc.dragonflyClient.ZRem(ctx, activeTACsKey, tgid)
		if err != nil {
			slog.Error("sweeper: failed to claim TGID", slog.String("error", err.Error()), slog.String("tgid", tgid))
			continue
		}
		if removed == 0 {
			continue
		}
		metaKey := fmt.Sprintf(tacMetaKeyFmt, tgid)
		raw, err := tc.dragonflyClient.Get(ctx, metaKey)
		if err != nil {
			slog.Error("sweeper: failed to read closure metadata", slog.String("error", err.Error()), slog.String("tgid", tgid))
			continue
		}

		// FIX (feedback URL prefill): cleanup MUST run after postChannelClosed, not before.
		// The feedback-URL builder inside updateAlertForClosure reads summary_data:<TGID>
		// for the headline + situation summary prefill — if Del runs first, those fields
		// silently fall back to empty in the form URL. Inline closure so each early-return
		// path also runs cleanup but the post-success path goes through it AFTER the post.
		cleanup := func() {
			_ = tc.dragonflyClient.Del(ctx,
				metaKey,
				fmt.Sprintf(tacTranscriptsKeyFmt, tgid),
				fmt.Sprintf(summaryTSKeyFmt, tgid),
				fmt.Sprintf(summaryLockKeyFmt, tgid),
				fmt.Sprintf(summaryStaleKeyFmt, tgid),
				fmt.Sprintf(summaryDataKeyFmt, tgid),
			)
		}

		if raw == "" {
			slog.Warn("sweeper: closure metadata missing, skipping", slog.String("tgid", tgid))
			cleanup()
			continue
		}
		var meta ClosureMeta
		if err := json.Unmarshal([]byte(raw), &meta); err != nil {
			slog.Error("sweeper: failed to unmarshal closure metadata, dropping", slog.String("error", err.Error()), slog.String("tgid", tgid), slog.String("raw", raw))
			cleanup()
			continue
		}
		tc.postChannelClosed(ctx, &meta)
		cleanup()
	}
}

func (tc *TranscribeClient) postChannelClosed(ctx context.Context, m *ClosureMeta) {
	closedAt := time.Now().Local()

	// 1) Rewrite the parent alert: same blocks, but the actions row is gone and the
	// "Expires …" line becomes "FTAC X monitoring auto-closed at …". We swallow update
	// errors (just log + continue) because the thread reply below is the canonical signal;
	// a stale parent message is a UX wart, not a correctness break.
	tc.updateAlertForClosure(ctx, m, closedAt)

	// 2) Post the channel-closed notice in the rescue thread.
	msgOptions := []slack.MsgOption{
		slack.MsgOptionBlocks(BuildChannelClosedBlocks(&ChannelClosedBlocksInput{
			Channel:  m.TACChannel,
			ClosedAt: closedAt,
		})...),
		slack.MsgOptionAsUser(true),
		slack.MsgOptionTS(m.ThreadTS),
	}
	if tc.config.SlackChannelClosedBroadcastEnabled {
		msgOptions = append(msgOptions, slack.MsgOptionBroadcast())
	}
	if _, err := tc.sendSlackWithRetry(ctx, m.SourceTalkgroup, msgOptions...); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			slog.Warn("sweeper: shutdown interrupted channel-closed post", slog.String("error", err.Error()), slog.String("tac", m.TACChannel))
			return
		}
		slog.Error("sweeper: failed to post channel closed", slog.String("error", err.Error()), slog.String("tac", m.TACChannel))
		return
	}
	// Info-level: a successful auto-close is a low-frequency, operationally-meaningful
	// event worth surfacing without raising the global log verbosity.
	slog.Info("sweeper: posted channel closed", slog.String("tac", m.TACChannel), slog.String("thread", m.ThreadTS))
}

// updateAlertForClosure rebuilds the parent alert with the actions block stripped and the
// expires-line replaced with an auto-closed line. Falls back gracefully if the original
// transcription wasn't stored (older entries in flight from before the field was added) —
// in that case the alert is just left as-is and the thread reply carries the signal.
func (tc *TranscribeClient) updateAlertForClosure(ctx context.Context, m *ClosureMeta, closedAt time.Time) {
	if m.MessageTS == "" || m.Transcription == "" {
		// MessageTS missing → no message to update. Transcription missing → we can't rebuild
		// the alert blocks faithfully without losing context; skip rather than producing a
		// bare "auto-closed" stub that strips the operative content.
		return
	}

	// Build the feedback URL BEFORE the closure cleanup runs (the sidecar Del happens in
	// the caller AFTER this method returns). buildFeedbackURL reads summary_data:<TGID>
	// for the headline + situation summary; it returns "" cleanly when FEEDBACK_FORM_URL
	// is not configured.
	feedbackURL := tc.buildFeedbackURL(ctx, m.TGID, *m, closedAt)

	blocks := BuildRescueTrailBlocks(&RescueTrailBlocksInput{
		TACChannel:        m.TACChannel,
		TranscriptionText: m.Transcription,
		// ExpiresAt is irrelevant in closed mode — the builder reads ClosedAt instead.
		DispatchTGID:     FireDispatch1TGID,
		TACTalkgroupTGID: m.TGID,
		ClosedAt:         &closedAt,
		FeedbackURL:      feedbackURL,
	})

	updateCtx, cancel := context.WithTimeout(ctx, tc.config.SlackTimeout)
	defer cancel()
	if _, _, _, err := tc.slackClient.UpdateMessageContext(updateCtx,
		tc.config.SlackChannelID,
		m.MessageTS,
		slack.MsgOptionBlocks(blocks...),
		// chat.update requires a fallback text; keep it terse so notification previews are
		// useful even though they're rare on already-seen messages.
		slack.MsgOptionText(fmt.Sprintf("%s monitoring auto-closed.", m.TACChannel), false),
	); err != nil {
		slog.Warn("sweeper: failed to update parent alert (thread reply still posts)",
			slog.String("error", err.Error()),
			slog.String("tac", m.TACChannel),
			slog.String("message_ts", m.MessageTS))
	}
}
