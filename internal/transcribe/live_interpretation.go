package transcribe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
)

// liveTranscriptEntry is the per-RPush record. Capture time is what the model wires into
// KeyEvents — pulled from the audio's filename timestamp, not when the transcription
// finished (which would jitter with pipeline latency).
type liveTranscriptEntry struct {
	CapturedAt string `json:"captured_at"`
	Text       string `json:"text"`
}

// updateLiveInterpretation appends one TAC transcript and refreshes the rescue thread's
// running summary. Best-effort: any failure logs and continues so the worker still acks the
// underlying Pulsar message — the canonical record of what was said is the per-transmission
// thread reply that processNonDispatchCall already posted.
//
// Concurrency model: when N transmissions arrive nearly simultaneously (synthetic trigger
// burst, real-world heavy traffic), each worker:
//  1. RPushes its transcript (everyone records, lossless).
//  2. Tries to SetNX a per-TGID summary lock. Loser sets a stale flag and returns immediately
//     — no LLM call, no Slack post.
//  3. Winner runs the summarize-and-post loop, which re-summarizes whenever the stale flag
//     was set during the last cycle. Net cost: ~2 LLM calls per burst regardless of N.
func (tc *TranscribeClient) updateLiveInterpretation(ctx context.Context, tacTGID string, capturedAt time.Time, transcript string) {
	if transcript == "" {
		return
	}

	listKey := fmt.Sprintf(tacTranscriptsKeyFmt, tacTGID)
	listTTL := 2 * tc.config.TacticalChannelActivationDuration
	if err := tc.appendTranscript(ctx, listKey, listTTL, capturedAt, transcript); err != nil {
		slog.Warn("live interpretation: append failed", slog.String("error", err.Error()), slog.String("tgid", tacTGID))
		return
	}

	// Try to take ownership of the LLM-and-post cycle. Losers mark the rescue stale and
	// return — the existing lock holder will pick up our transcript on its next pass.
	lockKey := fmt.Sprintf(summaryLockKeyFmt, tacTGID)
	acquired, err := tc.dragonflyClient.SetNX(ctx, lockKey, summaryLockTTL, "1")
	if err != nil {
		slog.Warn("live interpretation: lock SetNX failed; skipping summary update", slog.String("error", err.Error()), slog.String("tgid", tacTGID))
		return
	}
	if !acquired {
		// Another worker is mid-summary. Mark stale so it knows to re-summarize when it
		// finishes — guarantees our transcript ends up reflected in the displayed summary.
		if err := tc.dragonflyClient.Set(ctx, fmt.Sprintf(summaryStaleKeyFmt, tacTGID), summaryStaleTTL, "1"); err != nil {
			slog.Warn("live interpretation: failed to set stale flag", slog.String("error", err.Error()), slog.String("tgid", tacTGID))
		}
		return
	}
	defer func() {
		if err := tc.dragonflyClient.Del(ctx, lockKey); err != nil {
			slog.Warn("live interpretation: failed to release summary lock; will expire via TTL", slog.String("error", err.Error()), slog.String("tgid", tacTGID))
		}
	}()

	// Loop: each pass clears the stale flag, re-reads the full transcripts list, summarizes,
	// and posts/updates Slack. If the stale flag got set during the work (another worker
	// arrived), we go around again. Hard-cap iterations as belt-and-suspenders against any
	// pathological loop where the flag is being toggled forever.
	const maxIterations = 5
	for i := 0; i < maxIterations; i++ {
		if err := tc.dragonflyClient.Del(ctx, fmt.Sprintf(summaryStaleKeyFmt, tacTGID)); err != nil {
			// Failure to clear isn't fatal — worst case we run an extra summarize.
			slog.Warn("live interpretation: failed to clear stale flag", slog.String("error", err.Error()))
		}
		if !tc.runOneSummaryPass(ctx, tacTGID, listKey, listTTL) {
			// Pass returned false: rescue isn't live (no metadata) or unrecoverable error.
			return
		}
		stale, err := tc.dragonflyClient.Get(ctx, fmt.Sprintf(summaryStaleKeyFmt, tacTGID))
		if err != nil {
			slog.Warn("live interpretation: failed to read stale flag; assuming caught up", slog.String("error", err.Error()))
			return
		}
		if stale != "1" {
			return // No new transcripts arrived during our work — we're caught up.
		}
		slog.Debug("live interpretation: stale flag set during summary; re-running", slog.String("tgid", tacTGID), slog.Int("iteration", i+1))
	}
	slog.Warn("live interpretation: hit maxIterations; giving up to avoid infinite loop", slog.String("tgid", tacTGID))
}

// appendTranscript handles the RPush + Expire pair so the caller stays focused on the
// concurrency policy. Re-stamps the TTL on every push so an active rescue's transcripts
// list never expires under the rescue's feet.
func (tc *TranscribeClient) appendTranscript(ctx context.Context, listKey string, listTTL time.Duration, capturedAt time.Time, transcript string) error {
	encoded, err := json.Marshal(liveTranscriptEntry{
		CapturedAt: capturedAt.Format("15:04:05"),
		Text:       transcript,
	})
	if err != nil {
		return fmt.Errorf("marshal transcript entry: %w", err)
	}
	if err := tc.dragonflyClient.RPush(ctx, listKey, string(encoded)); err != nil {
		return fmt.Errorf("RPush: %w", err)
	}
	if err := tc.dragonflyClient.Expire(ctx, listKey, listTTL); err != nil {
		return fmt.Errorf("Expire: %w", err)
	}
	return nil
}

// runOneSummaryPass reads the full transcripts list, calls SummarizeRescue, and posts (or
// chat.update's) the running interpretation message. Returns false on terminal failures
// (no metadata, ML unrecoverable error) so the caller stops iterating.
func (tc *TranscribeClient) runOneSummaryPass(ctx context.Context, tacTGID, listKey string, listTTL time.Duration) bool {
	rawEntries, err := tc.dragonflyClient.LRange(ctx, listKey, 0, -1)
	if err != nil {
		slog.Warn("live interpretation: LRange failed", slog.String("error", err.Error()), slog.String("tgid", tacTGID))
		return false
	}
	transcripts := make([]ml.TACTranscript, 0, len(rawEntries))
	for _, raw := range rawEntries {
		var e liveTranscriptEntry
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			slog.Warn("live interpretation: dropping unparseable transcript entry", slog.String("error", err.Error()))
			continue
		}
		transcripts = append(transcripts, ml.TACTranscript{CapturedAt: e.CapturedAt, Text: e.Text})
	}

	meta, ok := tc.readClosureMeta(ctx, tacTGID)
	if !ok {
		return false
	}

	summary, err := tc.mlClient.SummarizeRescue(ctx, ml.RescueSummaryInput{
		DispatchTranscription: meta.Transcription,
		DispatchCallType:      "Rescue - Trail",
		TACChannel:            meta.TACChannel,
		TACTranscripts:        transcripts,
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			slog.Warn("live interpretation: shutdown interrupted summarize", slog.String("error", err.Error()))
			return false
		}
		slog.Error("live interpretation: SummarizeRescue failed", slog.String("error", err.Error()), slog.String("tgid", tacTGID))
		return false
	}

	tc.publishLiveInterpretation(ctx, tacTGID, meta, summary, listTTL)
	slog.Info("live interpretation: posted summary",
		slog.String("tgid", tacTGID),
		slog.Int("transcripts_count", len(transcripts)),
		slog.String("headline", summary.Headline))
	return true
}

// publishLiveInterpretation posts (or chat.updates) the running-summary message in the
// rescue thread. The message_ts is cached in summary_ts:<TGID> with the same TTL as the
// transcripts list so an active rescue keeps a stable summary anchor.
func (tc *TranscribeClient) publishLiveInterpretation(ctx context.Context, tacTGID string, meta ClosureMeta, summary *ml.RescueSummary, ttl time.Duration) {
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
