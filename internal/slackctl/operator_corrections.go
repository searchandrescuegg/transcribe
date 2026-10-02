package slackctl

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
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

// mentionCorrectionRE matches the opt-in marker: a message that STARTS with a mention of the bot
// (Slack sends "<@U123>" or the legacy "<@U123|name>"), optionally followed by ":". Requiring the
// mention up front keeps it deliberate — "lol @PSERN is wrong again" mid-sentence is chatter, not
// a correction. Group 1 is the user ID, group 2 the correction text.
var mentionCorrectionRE = regexp.MustCompile(`(?s)^\s*<@([A-Z0-9]+)(?:\|[^>]*)?>\s*:?(.*)$`)

// extractCorrection returns the correction text when text starts with a mention of botUserID.
// An empty botUserID (auth.test failed at startup) disables mention corrections entirely.
func extractCorrection(text, botUserID string) (string, bool) {
	if botUserID == "" {
		return "", false
	}
	m := mentionCorrectionRE.FindStringSubmatch(text)
	if m == nil || m[1] != botUserID {
		return "", false
	}
	body := strings.TrimSpace(m[2])
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
// it isn't one: wrong channel, not a thread reply, from a bot, or not starting with a mention of
// the bot. Edits that keep the leading mention upsert; edits that drop it, and deletes, remove.
func parseCorrectionEvent(ev *slackevents.MessageEvent, channelID, botUserID string) (correctionOp, bool) {
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
		text, ok := extractCorrection(m.Text, botUserID)
		if !ok {
			return correctionOp{}, false
		}
		return correctionOp{Kind: correctionOpUpsert, ThreadTS: m.ThreadTimestamp, MessageTS: m.Timestamp, UserID: m.User, Text: text}, true
	case "message_changed":
		m := ev.Message
		if !isThreadReply(m) {
			return correctionOp{}, false
		}
		if text, ok := extractCorrection(m.Text, botUserID); ok {
			return correctionOp{Kind: correctionOpUpsert, ThreadTS: m.ThreadTimestamp, MessageTS: m.Timestamp, UserID: m.User, Text: text}, true
		}
		if prev := ev.PreviousMessage; prev != nil {
			if _, was := extractCorrection(prev.Text, botUserID); was {
				return correctionOp{Kind: correctionOpRemove, ThreadTS: m.ThreadTimestamp, MessageTS: m.Timestamp, UserID: m.User}, true
			}
		}
		return correctionOp{}, false
	case "message_deleted":
		prev := ev.PreviousMessage
		if !isThreadReply(prev) {
			return correctionOp{}, false
		}
		if _, was := extractCorrection(prev.Text, botUserID); !was {
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
	op, ok := parseCorrectionEvent(msgEv, c.cfg.SlackChannelID, c.botUserID)
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

// correctionOpLockStripes is the number of striped mutexes serializing operations per Slack
// message. Collisions only cost a little unnecessary serialization; nothing ever leaks.
const correctionOpLockStripes = 64

// correctionOpLock returns the stripe guarding operations on the note posted as slackTS.
func (c *Controller) correctionOpLock(slackTS string) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(slackTS))
	return &c.correctionOpLocks[h.Sum32()%correctionOpLockStripes]
}

// handleCorrectionOp applies one operator-note (leading @-mention) operation. The stripe lock is scoped
// narrowly to the storage read-modify-write (Upsert/Remove) ONLY — that's the sole step where an
// edit racing a delete for the same message could read the live entry, lose the race, and write
// Deleted=false over the retraction (resurrecting it). react and RefreshLiveInterpretation run
// after the lock is released: refresh reads whatever state is current when it runs, so its
// ordering relative to other ops doesn't matter, but it can take minutes (up to 5 summary passes
// of ~120s each while it holds the per-TGID summary lock). Holding the stripe lock across it would
// stall any other op on the same message — or a same-stripe collision, ~1/64 of messages — behind
// a multi-minute refresh, risking that op's own WorkerTimeout-bounded context expiring in the
// queue and silently dropping an edit or retraction.
func (c *Controller) handleCorrectionOp(ctx context.Context, op correctionOp) {
	mu := c.correctionOpLock(op.MessageTS)

	switch op.Kind {
	case correctionOpUpsert:
		at := slackTSTime(op.MessageTS)
		mu.Lock()
		tgid, created, err := c.corrections.UpsertOperatorCorrection(ctx, op.ThreadTS, op.MessageTS, op.UserID, op.Text, at)
		mu.Unlock()
		if errors.Is(err, transcribe.ErrRescueNotActive) {
			return // not an active rescue thread — ignore
		}
		if errors.Is(err, transcribe.ErrOperatorCorrectionUnchanged) {
			return // same text re-sent (link unfurl etc.) — nothing changed, don't force a rewrite
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
		mu.Lock()
		tgid, removed, err := c.corrections.RemoveOperatorCorrection(ctx, op.ThreadTS, op.MessageTS, op.UserID)
		mu.Unlock()
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
