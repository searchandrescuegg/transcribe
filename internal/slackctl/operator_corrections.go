package slackctl

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/searchandrescuegg/transcribe/internal/transcribe"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

const (
	// slackEventDedupKeyFmt guards against Socket Mode redelivering the same Events API event.
	// Controller-only, TTL-only (not a per-TGID sidecar).
	slackEventDedupKeyFmt = "slack_event:%s"
	slackEventDedupTTL    = 10 * time.Minute
)

// correctionPrefixRE matches the opt-in marker: `correction:` (any case), leading whitespace
// allowed. Anything else in the thread — including jokes — is ignored.
var correctionPrefixRE = regexp.MustCompile(`(?is)^\s*correction\s*:(.*)$`)

func stripCorrectionPrefix(text string) (string, bool) {
	m := correctionPrefixRE.FindStringSubmatch(text)
	if m == nil {
		return "", false
	}
	body := strings.TrimSpace(m[1])
	return body, body != ""
}

type correctionOpKind int

const (
	correctionOpUpsert correctionOpKind = iota + 1
	correctionOpRemove
)

type correctionOp struct {
	Kind      correctionOpKind
	ThreadTS  string
	MessageTS string
	UserID    string
	Text      string
}

// parseCorrectionEvent turns a Slack `message` event into a correction operation, or false when
// it isn't one: wrong channel, not a thread reply, from a bot, or no `correction:` prefix.
// Edits that keep the prefix upsert; edits that drop it, and deletes, remove.
func parseCorrectionEvent(ev *slackevents.MessageEvent, channelID string) (correctionOp, bool) {
	if ev == nil || ev.Channel != channelID || ev.BotID != "" {
		return correctionOp{}, false
	}
	isThreadReply := func(m *slack.Msg) bool {
		return m != nil && m.ThreadTimestamp != "" && m.ThreadTimestamp != m.Timestamp && m.BotID == "" && m.SubType != "bot_message"
	}
	switch ev.SubType {
	case "", "thread_broadcast":
		m := ev.Message
		if !isThreadReply(m) {
			return correctionOp{}, false
		}
		text, ok := stripCorrectionPrefix(m.Text)
		if !ok {
			return correctionOp{}, false
		}
		return correctionOp{Kind: correctionOpUpsert, ThreadTS: m.ThreadTimestamp, MessageTS: m.Timestamp, UserID: m.User, Text: text}, true
	case "message_changed":
		m := ev.Message
		if !isThreadReply(m) {
			return correctionOp{}, false
		}
		if text, ok := stripCorrectionPrefix(m.Text); ok {
			return correctionOp{Kind: correctionOpUpsert, ThreadTS: m.ThreadTimestamp, MessageTS: m.Timestamp, UserID: m.User, Text: text}, true
		}
		if prev := ev.PreviousMessage; prev != nil {
			if _, was := stripCorrectionPrefix(prev.Text); was {
				return correctionOp{Kind: correctionOpRemove, ThreadTS: m.ThreadTimestamp, MessageTS: m.Timestamp, UserID: m.User}, true
			}
		}
		return correctionOp{}, false
	case "message_deleted":
		prev := ev.PreviousMessage
		if !isThreadReply(prev) {
			return correctionOp{}, false
		}
		if _, was := stripCorrectionPrefix(prev.Text); !was {
			return correctionOp{}, false
		}
		ts := ev.DeletedTimeStamp
		if ts == "" {
			ts = prev.Timestamp
		}
		return correctionOp{Kind: correctionOpRemove, ThreadTS: prev.ThreadTimestamp, MessageTS: ts, UserID: prev.User}, true
	default:
		return correctionOp{}, false
	}
}

// dispatchEvent handles Events API envelopes. Only `message` events that parse as corrections
// from allowlisted users do anything; everything else (incl. non-allowlisted users) is ignored
// silently by design.
func (c *Controller) dispatchEvent(evt *socketmode.Event, client *socketmode.Client) {
	if evt.Request != nil {
		client.Ack(*evt.Request)
	}
	if c.corrections == nil {
		return
	}
	apiEvt, ok := evt.Data.(slackevents.EventsAPIEvent)
	if !ok {
		return
	}
	msgEv, ok := apiEvt.InnerEvent.Data.(*slackevents.MessageEvent)
	if !ok {
		return
	}
	op, ok := parseCorrectionEvent(msgEv, c.cfg.SlackChannelID)
	if !ok || !c.isAuthorized(op.UserID) {
		return
	}

	ctx, cancel := c.workCtx()
	if cb, ok := apiEvt.Data.(*slackevents.EventsAPICallbackEvent); ok && cb.EventID != "" {
		first, err := c.dfly.SetNX(ctx, fmt.Sprintf(slackEventDedupKeyFmt, cb.EventID), slackEventDedupTTL, "1")
		if err == nil && !first {
			cancel()
			return // redelivery
		}
	}
	go func() {
		defer cancel()
		c.handleCorrectionOp(ctx, op)
	}()
}

func (c *Controller) handleCorrectionOp(ctx context.Context, op correctionOp) {
	switch op.Kind {
	case correctionOpUpsert:
		at := slackTSTime(op.MessageTS)
		tgid, created, err := c.corrections.UpsertOperatorCorrection(ctx, op.ThreadTS, op.MessageTS, op.UserID, op.Text, at)
		if errors.Is(err, transcribe.ErrRescueNotActive) {
			return // not an active rescue thread — ignore
		}
		if err != nil {
			slog.Warn("slackctl: store correction failed", slog.String("error", err.Error()), slog.String("ts", op.MessageTS))
			c.react(ctx, op.MessageTS, "warning")
			return
		}
		if created {
			c.react(ctx, op.MessageTS, "white_check_mark")
		}
		c.corrections.RefreshLiveInterpretation(ctx, tgid, !created)
	case correctionOpRemove:
		tgid, removed, err := c.corrections.RemoveOperatorCorrection(ctx, op.ThreadTS, op.MessageTS, op.UserID)
		if err != nil {
			slog.Warn("slackctl: retract correction failed", slog.String("error", err.Error()), slog.String("ts", op.MessageTS))
			return
		}
		if removed {
			c.corrections.RefreshLiveInterpretation(ctx, tgid, true)
		}
	}
}

func (c *Controller) react(ctx context.Context, ts, name string) {
	if err := c.slackClient.AddReactionContext(ctx, name, slack.NewRefToMessage(c.cfg.SlackChannelID, ts)); err != nil {
		slog.Warn("slackctl: add reaction failed", slog.String("error", err.Error()), slog.String("reaction", name))
	}
}

// slackTSTime converts a Slack ts ("1759012345.000200") to its wall-clock time; falls back to now.
func slackTSTime(ts string) time.Time {
	secs, err := strconv.ParseFloat(ts, 64)
	if err != nil {
		return time.Now()
	}
	return time.Unix(int64(secs), 0)
}
