package slackctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/searchandrescuegg/transcribe/internal/transcribe"
	"github.com/slack-go/slack"
)

// allowedTalkgroupsKey and tgRoutingKeyFmt mirror the keys written by the transcribe
// service. Kept here as constants (instead of importing from internal/transcribe) so a
// schema change is visible at the controller layer too — both sides need to agree.
const (
	allowedTalkgroupsKey = "allowed_talkgroups"
	tgRoutingKeyFmt      = "tg:%s"
	tacMetaKeyFmt        = "tac_meta:%s"
	activeTACsKey        = "active_tacs"
	// Live-interpretation sidecars; mirror constants in internal/transcribe/live_interpretation.go.
	// Cancel and Switch must clear them so a closed-then-reopened rescue doesn't inherit
	// stale transcripts from the previous incident.
	tacTranscriptsKeyFmt = "tac_transcripts:%s"
	summaryTSKeyFmt      = "summary_ts:%s"
	summaryLockKeyFmt    = "summary_lock:%s"
	summaryStaleKeyFmt   = "summary_stale:%s"
	summaryDataKeyFmt    = "summary_data:%s"
)

// CancelTAC performs the state mutations for a Cancel / False Alarm action. It is
// independent of Slack so it can be unit-tested directly against Dragonfly. The returned
// metadata is used by the Slack-side handler to post a status message in the original
// thread.
//
// Effects, in order:
//  1. Remove the TGID from `allowed_talkgroups` so further TAC transmissions are dropped
//     by the rules.IsObjectAllowed check.
//  2. Delete the `tg:<TGID>` routing key so processNonDispatchCall returns a clean
//     "thread ID empty" error rather than posting into a channel that's been cancelled.
//  3. Remove the pending closure from the `active_tacs` ZSET so the sweeper doesn't fire
//     a "channel closed" message after a cancellation already announced the close.
//  4. Delete the metadata sibling key.
//
// Returns ok=false (no error) when the TAC was not currently active (e.g. another worker
// already cancelled, or the TAC has already auto-expired). Callers surface this to the
// user as "no longer active" rather than as a failure.
func (c *Controller) CancelTAC(ctx context.Context, tgid string) (meta transcribe.ClosureMeta, ok bool, err error) {
	if tgid == "" {
		return transcribe.ClosureMeta{}, false, errors.New("CancelTAC: TGID is required")
	}

	// Read the metadata first so we can return it even if some of the deletes fail later.
	// This is best-effort — if the metadata key is gone the TAC has likely already expired.
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
	if _, err := c.dfly.ZRem(ctx, activeTACsKey, tgid); err != nil {
		return meta, false, fmt.Errorf("ZRem active_tacs: %w", err)
	}
	if err := c.dfly.Del(ctx,
		fmt.Sprintf(tacMetaKeyFmt, tgid),
		fmt.Sprintf(tacTranscriptsKeyFmt, tgid),
		fmt.Sprintf(summaryTSKeyFmt, tgid),
		fmt.Sprintf(summaryLockKeyFmt, tgid),
		fmt.Sprintf(summaryStaleKeyFmt, tgid),
		fmt.Sprintf(summaryDataKeyFmt, tgid),
	); err != nil {
		return meta, false, fmt.Errorf("del closure sidecars: %w", err)
	}
	return meta, true, nil
}

func (c *Controller) readClosureMeta(ctx context.Context, tgid string) (transcribe.ClosureMeta, bool, error) {
	raw, err := c.dfly.Get(ctx, fmt.Sprintf(tacMetaKeyFmt, tgid))
	if err != nil {
		return transcribe.ClosureMeta{}, false, fmt.Errorf("get tac_meta:<TGID>: %w", err)
	}
	if raw == "" {
		return transcribe.ClosureMeta{}, false, nil
	}
	var meta transcribe.ClosureMeta
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		return transcribe.ClosureMeta{}, false, fmt.Errorf("unmarshal tac_meta:<TGID>: %w", err)
	}
	return meta, true, nil
}

func (c *Controller) handleCancel(ctx context.Context, payload slack.InteractionCallback, action *slack.BlockAction) {
	tgid := action.Value
	meta, ok, err := c.CancelTAC(ctx, tgid)
	if err != nil {
		slog.Error("slackctl: cancel state mutation failed", slog.String("error", err.Error()), slog.String("tgid", tgid), slog.String("user", payload.User.ID))
		c.postEphemeral(payload, ":warning: Cancel failed; check service logs.")
		return
	}
	if !ok {
		c.postEphemeral(payload, ":information_source: This TAC monitoring window is no longer active (already cancelled or auto-expired).")
		return
	}

	slog.Info("slackctl: cancelled TAC monitoring",
		slog.String("user", payload.User.ID),
		slog.String("user_name", payload.User.Name),
		slog.String("tgid", tgid),
		slog.String("tac_channel", meta.TACChannel),
	)

	// 1) Post a thread reply announcing the cancellation. <@USERID> renders as a Slack
	// mention in clients, giving an audit trail of who pulled the trigger.
	threadMsg := fmt.Sprintf(":octagonal_sign: %s monitoring cancelled by <@%s> (false alarm) at %s.",
		meta.TACChannel, payload.User.ID, time.Now().Local().Format("15:04 MST"))
	_, _, _, err = c.slackClient.SendMessageContext(ctx,
		c.cfg.SlackChannelID,
		slack.MsgOptionText(threadMsg, false),
		slack.MsgOptionTS(meta.ThreadTS),
		slack.MsgOptionAsUser(true),
	)
	if err != nil {
		slog.Error("slackctl: failed to post cancellation thread reply", slog.String("error", err.Error()))
	}

	// 2) Update the original alert message: strip the actions block and add a "cancelled"
	// note so the buttons can't be pressed twice and the audit context lives with the alert.
	c.replaceAlertWithCancelNotice(ctx, payload, meta)
}

// replaceAlertWithCancelNotice rewrites the parent alert in place so the action buttons
// disappear. Slack's chat.update preserves the message thread; this is purely about UX.
func (c *Controller) replaceAlertWithCancelNotice(ctx context.Context, payload slack.InteractionCallback, meta transcribe.ClosureMeta) {
	noticeBlocks := []slack.Block{
		slack.NewHeaderBlock(
			slack.NewTextBlockObject(slack.PlainTextType, "Rescue Trail — Cancelled", true, false),
		),
		slack.NewDividerBlock(),
		slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType,
				fmt.Sprintf("*%s* monitoring was cancelled by <@%s> at %s (false alarm).",
					meta.TACChannel, payload.User.ID, time.Now().Local().Format("01/02/06 15:04 MST")),
				false, false),
			nil, nil,
		),
	}
	_, _, _, err := c.slackClient.UpdateMessageContext(ctx,
		payload.Container.ChannelID,
		meta.MessageTS,
		slack.MsgOptionBlocks(noticeBlocks...),
		// chat.update requires a fallback text. Keep it in the same voice as the blocks.
		slack.MsgOptionText(fmt.Sprintf("%s monitoring cancelled (false alarm).", meta.TACChannel), false),
	)
	if err != nil {
		slog.Error("slackctl: failed to update alert message after cancel", slog.String("error", err.Error()))
	}
}
