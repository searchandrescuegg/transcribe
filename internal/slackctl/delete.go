package slackctl

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/slack-go/slack"
)

// handleDelete removes the specific alert MESSAGE the Delete button was clicked on. It always
// targets payload.Container.MessageTs (the clicked message), so an orphaned duplicate can be
// removed without disturbing the live alert that shares its TGID.
//
// Smart teardown: if the clicked message IS the live alert (its ts matches tac_meta.MessageTS),
// deleting it also tears the incident down via CancelTAC (SREM allow-list + DEL sidecars + ZREM)
// so a false-positive alert doesn't keep monitoring its TAC in the background. If it's an orphan
// (ts differs) or the incident is already gone, only the message is removed and any live incident
// is left untouched.
func (c *Controller) handleDelete(ctx context.Context, payload slack.InteractionCallback, action *slack.BlockAction) {
	tgid := action.Value
	channelID := payload.Container.ChannelID
	msgTS := payload.Container.MessageTs

	if channelID == "" || msgTS == "" {
		slog.Warn("slackctl: delete missing message reference",
			slog.String("tgid", tgid), slog.String("channel", channelID), slog.String("message_ts", msgTS))
		c.postEphemeral(payload, ":warning: Delete failed — couldn't identify which message to remove.")
		return
	}

	// Determine whether this is the live alert. A read error defaults to "not live" so we never
	// wrongly tear down an incident we couldn't verify — we just remove the clicked message.
	meta, ok, err := c.readClosureMeta(ctx, tgid)
	if err != nil {
		slog.Error("slackctl: delete could not read closure meta; treating as message-only",
			slog.String("error", err.Error()), slog.String("tgid", tgid))
		ok = false
	}
	isLiveAlert := ok && meta.MessageTS == msgTS

	if isLiveAlert {
		if _, _, cerr := c.CancelTAC(ctx, tgid); cerr != nil {
			slog.Error("slackctl: delete teardown failed",
				slog.String("error", cerr.Error()), slog.String("tgid", tgid), slog.String("user", payload.User.ID))
			c.postEphemeral(payload, ":warning: Delete failed while stopping monitoring; check service logs.")
			return
		}
	}

	if _, _, derr := c.slackClient.DeleteMessageContext(ctx, channelID, msgTS); derr != nil {
		slog.Error("slackctl: chat.delete failed",
			slog.String("error", derr.Error()), slog.String("tgid", tgid), slog.String("message_ts", msgTS))
		c.postEphemeral(payload, ":warning: Couldn't delete the message (Slack permissions?). Check service logs.")
		return
	}

	if isLiveAlert {
		slog.Info("slackctl: deleted live rescue alert and stopped monitoring",
			slog.String("user", payload.User.ID), slog.String("user_name", payload.User.Name),
			slog.String("tgid", tgid), slog.String("tac_channel", meta.TACChannel))
		c.postEphemeral(payload, fmt.Sprintf(":wastebasket: Deleted the live alert for %s and stopped monitoring.", meta.TACChannel))
		return
	}

	slog.Info("slackctl: deleted orphaned/duplicate rescue alert",
		slog.String("user", payload.User.ID), slog.String("user_name", payload.User.Name),
		slog.String("tgid", tgid), slog.String("message_ts", msgTS))
	c.postEphemeral(payload, ":wastebasket: Deleted this alert message. Any live rescue on this channel is unaffected.")
}
