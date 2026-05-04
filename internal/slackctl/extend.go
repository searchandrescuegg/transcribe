package slackctl

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/searchandrescuegg/transcribe/internal/transcribe"
	"github.com/slack-go/slack"
)

// ExtendTAC pushes the auto-close out by another full TacticalChannelActivationDuration.
// It is independent of Slack so it can be unit-tested directly against Dragonfly.
//
// Effects, in order:
//  1. SAddEx the TGID into `allowed_talkgroups` again — Dragonfly's per-member TTL is
//     replaced when the same member is re-added, so this functions as a TTL refresh.
//  2. Re-Set the `tg:<TGID>` routing key with the existing thread_ts and the new TTL.
//  3. ZAdd the TGID to `active_tacs` with the new score (ZAdd updates the score when the
//     member already exists).
//  4. The metadata sibling does not need to change; its safety-net TTL is plenty long.
//
// Returns the new expiry time so the caller can render it in the Slack message, plus the
// closure metadata so the caller can post a thread reply identifying the TAC.
func (c *Controller) ExtendTAC(ctx context.Context, tgid string) (newExpiry time.Time, meta transcribe.ClosureMeta, ok bool, err error) {
	if tgid == "" {
		return time.Time{}, transcribe.ClosureMeta{}, false, errors.New("ExtendTAC: TGID is required")
	}

	meta, found, err := c.readClosureMeta(ctx, tgid)
	if err != nil {
		return time.Time{}, transcribe.ClosureMeta{}, false, err
	}
	if !found {
		return time.Time{}, transcribe.ClosureMeta{}, false, nil
	}

	dur := c.cfg.TacticalChannelActivationDuration
	newExpiry = time.Now().Add(dur)

	if err := c.dfly.SAddEx(ctx, allowedTalkgroupsKey, dur, tgid); err != nil {
		return time.Time{}, meta, false, fmt.Errorf("SAddEx allowed_talkgroups: %w", err)
	}
	// Refresh the routing key with the same thread_ts and the new TTL. Set is unconditional
	// so we don't need a GET first; the value we want is already in meta.ThreadTS.
	if err := c.dfly.Set(ctx, fmt.Sprintf(tgRoutingKeyFmt, tgid), dur, meta.ThreadTS); err != nil {
		return time.Time{}, meta, false, fmt.Errorf("Set tg:<TGID>: %w", err)
	}
	if err := c.dfly.ZAdd(ctx, activeTACsKey, float64(newExpiry.Unix()), tgid); err != nil {
		return time.Time{}, meta, false, fmt.Errorf("ZAdd active_tacs: %w", err)
	}
	return newExpiry, meta, true, nil
}

func (c *Controller) handleExtend(ctx context.Context, payload slack.InteractionCallback, action *slack.BlockAction) {
	tgid := action.Value
	newExpiry, meta, ok, err := c.ExtendTAC(ctx, tgid)
	if err != nil {
		slog.Error("slackctl: extend state mutation failed", slog.String("error", err.Error()), slog.String("tgid", tgid), slog.String("user", payload.User.ID))
		c.postEphemeral(payload, ":warning: Extend failed; check service logs.")
		return
	}
	if !ok {
		c.postEphemeral(payload, ":information_source: This TAC monitoring window is no longer active (already cancelled or auto-expired).")
		return
	}

	slog.Info("slackctl: extended TAC monitoring",
		slog.String("user", payload.User.ID),
		slog.String("user_name", payload.User.Name),
		slog.String("tgid", tgid),
		slog.String("tac_channel", meta.TACChannel),
		slog.Time("new_expiry", newExpiry),
	)

	threadMsg := fmt.Sprintf(":hourglass_flowing_sand: %s monitoring extended by <@%s> until %s.",
		meta.TACChannel, payload.User.ID, newExpiry.Local().Format("01/02/06 15:04 MST"))
	_, _, _, err = c.slackClient.SendMessageContext(ctx,
		c.cfg.SlackChannelID,
		slack.MsgOptionText(threadMsg, false),
		slack.MsgOptionTS(meta.ThreadTS),
		slack.MsgOptionAsUser(true),
	)
	if err != nil {
		slog.Error("slackctl: failed to post extension thread reply", slog.String("error", err.Error()))
	}

	// We deliberately don't chat.update the original alert here. Rebuilding it requires the
	// full transcription text, which we don't keep in metadata. The thread reply is the
	// canonical record of the new expiry; the original alert's "Expires …" text becomes
	// stale but the buttons remain functional for further actions.
}
