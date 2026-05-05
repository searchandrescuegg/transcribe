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

// CloseTAC ends the rescue early but routes the closure through the same path the auto-
// expiry sweeper takes. Distinct from CancelTAC (false-alarm), which wipes the alert and
// summary context. Close preserves the structured Live Interpretation, rewrites the
// parent alert with the Submit Feedback button, and posts a normal "Channel Closed"
// reply — identical to a natural expiry.
//
// Effects, in order:
//  1. Read closure metadata for the TGID; ok=false (no error) when no longer active.
//  2. SREM the TGID from allowed_talkgroups so further TAC traffic is rejected
//     immediately rather than waiting for the per-member SADDEX TTL to lapse.
//  3. DEL tg:<TGID> for the same reason — defensive routing cutoff.
//  4. ZADD active_tacs with score = now-1 so the sweeper claims it on its next tick
//     (~5s) and runs postChannelClosed + updateAlertForClosure + sidecar cleanup.
//
// We deliberately do NOT touch tac_meta, tac_transcripts, summary_*, or call ZRem on
// active_tacs — the sweeper owns those deletions, and clearing summary_data prematurely
// would silently strip the feedback URL prefill (same hazard as invariant #4 in CLAUDE.md).
func (c *Controller) CloseTAC(ctx context.Context, tgid string) (meta transcribe.ClosureMeta, ok bool, err error) {
	if tgid == "" {
		return transcribe.ClosureMeta{}, false, errors.New("CloseTAC: TGID is required")
	}
	meta, found, err := c.readClosureMeta(ctx, tgid)
	if err != nil {
		return transcribe.ClosureMeta{}, false, err
	}
	if !found {
		return transcribe.ClosureMeta{}, false, nil
	}

	if err := c.dfly.SRem(ctx, allowedTalkgroupsKey, tgid); err != nil {
		return meta, false, fmt.Errorf("SRem allowed_talkgroups: %w", err)
	}
	if err := c.dfly.Del(ctx, fmt.Sprintf(tgRoutingKeyFmt, tgid)); err != nil {
		return meta, false, fmt.Errorf("del tg:<TGID>: %w", err)
	}
	// ZAdd updates the score for an existing member, so the original future-dated entry
	// is moved into the past and picked up on the next sweeper tick.
	triggerScore := float64(time.Now().Unix() - 1)
	if err := c.dfly.ZAdd(ctx, activeTACsKey, triggerScore, tgid); err != nil {
		return meta, false, fmt.Errorf("ZAdd active_tacs trigger: %w", err)
	}
	return meta, true, nil
}

func (c *Controller) handleClose(ctx context.Context, payload slack.InteractionCallback, action *slack.BlockAction) {
	tgid := action.Value
	meta, ok, err := c.CloseTAC(ctx, tgid)
	if err != nil {
		slog.Error("slackctl: close state mutation failed", slog.String("error", err.Error()), slog.String("tgid", tgid), slog.String("user", payload.User.ID))
		c.postEphemeral(payload, ":warning: Close failed; check service logs.")
		return
	}
	if !ok {
		c.postEphemeral(payload, ":information_source: This TAC monitoring window is no longer active (already cancelled or auto-expired).")
		return
	}

	slog.Info("slackctl: closed TAC monitoring early",
		slog.String("user", payload.User.ID),
		slog.String("user_name", payload.User.Name),
		slog.String("tgid", tgid),
		slog.String("tac_channel", meta.TACChannel),
	)

	// Brief attribution reply for the audit trail. The sweeper will follow up within
	// ~TACSweeperInterval with the canonical "Channel Closed" message and rewrite the
	// parent alert (including the Submit Feedback button).
	threadMsg := fmt.Sprintf(":white_check_mark: %s monitoring closed by <@%s> at %s.",
		meta.TACChannel, payload.User.ID, time.Now().Local().Format("15:04 MST"))
	if _, _, _, err := c.slackClient.SendMessageContext(ctx,
		c.cfg.SlackChannelID,
		slack.MsgOptionText(threadMsg, false),
		slack.MsgOptionTS(meta.ThreadTS),
		slack.MsgOptionAsUser(true),
	); err != nil {
		slog.Error("slackctl: failed to post close thread reply", slog.String("error", err.Error()))
	}
}
