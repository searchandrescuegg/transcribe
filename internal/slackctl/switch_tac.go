package slackctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/searchandrescuegg/transcribe/internal/transcribe"
	"github.com/slack-go/slack"
)

// ErrSwitchSameTAC means the user picked the TAC the rescue is already on. Surfaced as an
// ephemeral "no change" message rather than a hard error.
var ErrSwitchSameTAC = errors.New("switch target equals current TAC")

// SwitchTAC migrates the active rescue's allow-list / routing / closure state from oldTGID
// to newTGID. Used by the Slack Switch-TAC dropdown when the LLM picked the wrong TAC and
// leadership corrects it.
//
// Effects, in order:
//  1. Read the closure metadata for oldTGID; if missing, the rescue is no longer active and
//     we return ok=false (caller surfaces "TAC monitoring no longer active").
//  2. Refuse if newTGID == oldTGID — same-TAC pick is a no-op.
//  3. Update tac_meta:<newTGID> with a copy of the metadata pointing at the new TAC.
//  4. SREM oldTGID from allowed_talkgroups; SAddEx newTGID with a fresh activation window.
//  5. DEL tg:<oldTGID>; SET tg:<newTGID> = thread_ts (same TTL).
//  6. ZREM oldTGID from active_tacs; ZADD newTGID at now+activation.
//  7. DEL tac_meta:<oldTGID>.
//
// Returns the updated metadata (with TGID/TACChannel set to the new values) plus the new
// expiry so the Slack-side handler can include the new auto-close time in the thread reply.
func (c *Controller) SwitchTAC(ctx context.Context, oldTGID, newTGID string) (newMeta transcribe.ClosureMeta, newExpiry time.Time, ok bool, err error) {
	if oldTGID == "" || newTGID == "" {
		return transcribe.ClosureMeta{}, time.Time{}, false, errors.New("SwitchTAC: oldTGID and newTGID are required")
	}
	if oldTGID == newTGID {
		return transcribe.ClosureMeta{}, time.Time{}, false, ErrSwitchSameTAC
	}

	oldMeta, found, err := c.readClosureMeta(ctx, oldTGID)
	if err != nil {
		return transcribe.ClosureMeta{}, time.Time{}, false, fmt.Errorf("read closure meta: %w", err)
	}
	if !found {
		return transcribe.ClosureMeta{}, time.Time{}, false, nil
	}

	// Resolve the new channel's short code (TAC1, TAC2, ...) from the canonical talkgroup
	// table so the metadata records human-readable identity, not just a TGID.
	newChannel, err := c.shortCodeForTGID(newTGID)
	if err != nil {
		return transcribe.ClosureMeta{}, time.Time{}, false, fmt.Errorf("resolve new TAC: %w", err)
	}

	dur := c.cfg.TacticalChannelActivationDuration
	newExpiry = time.Now().Add(dur)

	newMeta = transcribe.ClosureMeta{
		TGID:            newTGID,
		TACChannel:      newChannel,
		ThreadTS:        oldMeta.ThreadTS,
		SourceTalkgroup: oldMeta.SourceTalkgroup,
		MessageTS:       oldMeta.MessageTS,
	}

	// Order: write new state BEFORE removing old. This means a reader observing mid-flight
	// state sees both (which is fine; allowed_talkgroups membership is the only thing that
	// matters for routing, and "both allowed" is harmless), never neither.
	if err := c.scheduleClosure(ctx, newMeta, newExpiry); err != nil {
		return transcribe.ClosureMeta{}, time.Time{}, false, fmt.Errorf("schedule new closure: %w", err)
	}
	if err := c.dfly.SAddEx(ctx, allowedTalkgroupsKey, dur, newTGID); err != nil {
		return transcribe.ClosureMeta{}, time.Time{}, false, fmt.Errorf("SAddEx new allowed: %w", err)
	}
	if err := c.dfly.Set(ctx, fmt.Sprintf(tgRoutingKeyFmt, newTGID), dur, oldMeta.ThreadTS); err != nil {
		return transcribe.ClosureMeta{}, time.Time{}, false, fmt.Errorf("Set new routing: %w", err)
	}

	// Now tear down the old state.
	if err := c.dfly.SRem(ctx, allowedTalkgroupsKey, oldTGID); err != nil {
		return transcribe.ClosureMeta{}, time.Time{}, false, fmt.Errorf("SRem old allowed: %w", err)
	}
	if err := c.dfly.Del(ctx, fmt.Sprintf(tgRoutingKeyFmt, oldTGID)); err != nil {
		return transcribe.ClosureMeta{}, time.Time{}, false, fmt.Errorf("Del old routing: %w", err)
	}
	if _, err := c.dfly.ZRem(ctx, activeTACsKey, oldTGID); err != nil {
		return transcribe.ClosureMeta{}, time.Time{}, false, fmt.Errorf("ZRem old closure: %w", err)
	}
	if err := c.dfly.Del(ctx,
		fmt.Sprintf(tacMetaKeyFmt, oldTGID),
		fmt.Sprintf(tacTranscriptsKeyFmt, oldTGID),
		fmt.Sprintf(summaryTSKeyFmt, oldTGID),
		fmt.Sprintf(summaryLockKeyFmt, oldTGID),
		fmt.Sprintf(summaryStaleKeyFmt, oldTGID),
		fmt.Sprintf(summaryDataKeyFmt, oldTGID),
	); err != nil {
		return transcribe.ClosureMeta{}, time.Time{}, false, fmt.Errorf("Del old closure sidecars: %w", err)
	}
	return newMeta, newExpiry, true, nil
}

// scheduleClosure writes both the metadata key and the ZSET entry for the new TAC.
// Mirrors transcribe.TranscribeClient.ScheduleTACClosure but stays inside the slackctl
// package so the controller doesn't need a TranscribeClient handle.
func (c *Controller) scheduleClosure(ctx context.Context, meta transcribe.ClosureMeta, expiresAt time.Time) error {
	payload, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal new closure meta: %w", err)
	}
	if err := c.dfly.Set(ctx, fmt.Sprintf(tacMetaKeyFmt, meta.TGID), 24*time.Hour, string(payload)); err != nil {
		return err
	}
	return c.dfly.ZAdd(ctx, activeTACsKey, float64(expiresAt.Unix()), meta.TGID)
}

// shortCodeForTGID resolves a TGID to its TAC1/TAC2/... short code via the canonical
// talkgroup table. Returns an error for unknown TGIDs (e.g. fire-dispatch 1399 isn't a
// valid switch target — this method is only ever called with the user's selected TAC
// option, which the dropdown limits to TAC1-TAC10).
func (c *Controller) shortCodeForTGID(tgid string) (string, error) {
	tg, ok := transcribe.TalkgroupByTGID(tgid)
	if !ok {
		return "", fmt.Errorf("unknown TGID %q", tgid)
	}
	return tg.RadioShortCode, nil
}

// parseOldTGIDFromBlockID extracts "1389" from "rescue_actions:1389". The block_id is
// where the active TGID is stamped at render time; the select element's per-option value
// carries the target TGID.
func parseOldTGIDFromBlockID(blockID string) (string, bool) {
	const prefix = "rescue_actions:"
	if !strings.HasPrefix(blockID, prefix) {
		return "", false
	}
	tgid := strings.TrimPrefix(blockID, prefix)
	return tgid, tgid != ""
}

func (c *Controller) handleSwitchTAC(ctx context.Context, payload slack.InteractionCallback, action *slack.BlockAction) {
	oldTGID, ok := parseOldTGIDFromBlockID(action.BlockID)
	if !ok {
		slog.Warn("slackctl: switch_tac action has unparseable block_id", slog.String("block_id", action.BlockID))
		c.postEphemeral(payload, ":warning: Switch failed (malformed action). Check service logs.")
		return
	}
	newTGID := action.SelectedOption.Value
	if newTGID == "" {
		slog.Warn("slackctl: switch_tac action has empty target TGID")
		c.postEphemeral(payload, ":warning: No new TAC selected.")
		return
	}

	newMeta, newExpiry, ok2, err := c.SwitchTAC(ctx, oldTGID, newTGID)
	switch {
	case errors.Is(err, ErrSwitchSameTAC):
		c.postEphemeral(payload, fmt.Sprintf(":information_source: Already monitoring %s — no change.", newMeta.TACChannel))
		return
	case err != nil:
		slog.Error("slackctl: switch state mutation failed",
			slog.String("error", err.Error()),
			slog.String("old_tgid", oldTGID),
			slog.String("new_tgid", newTGID),
			slog.String("user", payload.User.ID))
		c.postEphemeral(payload, ":warning: Switch failed; check service logs.")
		return
	case !ok2:
		c.postEphemeral(payload, ":information_source: This rescue is no longer active (already cancelled or auto-expired).")
		return
	}

	oldChannel, _ := c.shortCodeForTGID(oldTGID)
	if oldChannel == "" {
		oldChannel = oldTGID
	}

	slog.Info("slackctl: switched TAC monitoring",
		slog.String("user", payload.User.ID),
		slog.String("user_name", payload.User.Name),
		slog.String("old_tgid", oldTGID),
		slog.String("new_tgid", newTGID),
		slog.String("old_tac", oldChannel),
		slog.String("new_tac", newMeta.TACChannel))

	threadMsg := fmt.Sprintf(":arrows_counterclockwise: Monitoring switched from %s to *%s* by <@%s>. New auto-close at %s.",
		oldChannel, newMeta.TACChannel, payload.User.ID, newExpiry.Local().Format("01/02/06 15:04 MST"))
	if _, _, _, err := c.slackClient.SendMessageContext(ctx,
		c.cfg.SlackChannelID,
		slack.MsgOptionText(threadMsg, false),
		slack.MsgOptionTS(newMeta.ThreadTS),
		slack.MsgOptionAsUser(true),
	); err != nil {
		slog.Error("slackctl: failed to post switch thread reply", slog.String("error", err.Error()))
	}

	// Note: we deliberately don't chat.update the original alert — rebuilding its blocks
	// would need the original transcription text which we don't persist in metadata. The
	// thread reply is the canonical record of the correction.
}
