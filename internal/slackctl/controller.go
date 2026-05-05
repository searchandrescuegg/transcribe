// Package slackctl runs the Slack interactivity controller. It opens an outbound Socket
// Mode WebSocket to Slack so leadership can press Cancel / Extend buttons on rescue-trail
// alerts without exposing a public HTTP endpoint. All state mutations land in Dragonfly,
// reusing the same keys the transcribe service writes — see internal/transcribe/sweeper.go.
package slackctl

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/searchandrescuegg/transcribe/internal/config"
	"github.com/searchandrescuegg/transcribe/internal/dragonfly"
	"github.com/searchandrescuegg/transcribe/internal/transcribe"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
)

// ErrSocketModeDisabled is returned when SLACK_APP_TOKEN is unset. main.go uses this to
// decide whether to launch the controller or skip the feature entirely.
var ErrSocketModeDisabled = errors.New("slackctl: SLACK_APP_TOKEN not set; interactivity disabled")

// Controller wires Socket Mode events to the cancel/extend state mutations. It is
// independent of TranscribeClient but shares the same Dragonfly keys.
type Controller struct {
	smClient    *socketmode.Client
	slackClient *slack.Client
	dfly        *dragonfly.DragonflyClient
	cfg         *config.Config

	// allowed is the membership set of Slack user IDs permitted to act on alerts. Built
	// once at construction; refreshing requires a service restart, which is a deliberate
	// trade-off to keep authorization auditable from one place (the env / secret manager).
	allowed map[string]struct{}
	// allowAny is set when SLACK_ALLOWED_USER_IDS contains the wildcard "*", meaning every
	// Slack user who can see the alert is permitted to press the buttons. This skips the
	// leadership gate entirely — operators choosing this should know what they're trading
	// off (lost audit-trail-as-authz, fat-finger surface area).
	allowAny bool
}

// New constructs the controller. Returns ErrSocketModeDisabled if SLACK_APP_TOKEN is
// empty so callers can no-op gracefully when the feature isn't configured.
func New(cfg *config.Config, dfly *dragonfly.DragonflyClient) (*Controller, error) {
	if cfg.SlackAppToken == "" {
		return nil, ErrSocketModeDisabled
	}
	if cfg.SlackToken == "" {
		return nil, errors.New("slackctl: SLACK_TOKEN required when SLACK_APP_TOKEN is set")
	}

	api := slack.New(cfg.SlackToken,
		slack.OptionAppLevelToken(cfg.SlackAppToken),
	)
	sm := socketmode.New(api)

	allowed := make(map[string]struct{}, len(cfg.SlackAllowedUserIDs))
	var allowAny bool
	for _, id := range cfg.SlackAllowedUserIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if id == "*" {
			// Wildcard: anyone in the channel can press the buttons. Other entries in the
			// same list become redundant but don't conflict — leave them populated in
			// `allowed` so a future operator removing the * still has the original list.
			allowAny = true
			continue
		}
		allowed[id] = struct{}{}
	}

	switch {
	case allowAny:
		// Distinct WARN so it shows up in operator log-scrapes — disabling the leadership
		// gate is a security-relevant choice that should never happen by accident.
		slog.Warn("slackctl: SLACK_ALLOWED_USER_IDS contains '*'; ALL channel members can press the rescue-control buttons (Cancel / Extend / Switch). No per-user authorization.")
	case len(allowed) == 0:
		// Not a hard error — there are legitimate reasons to launch the bot before granting
		// anyone access (e.g. pre-deploy smoke test). Just make it loud so this isn't missed.
		slog.Warn("slackctl: SLACK_ALLOWED_USER_IDS is empty; nobody can press the buttons")
	}

	return &Controller{
		smClient:    sm,
		slackClient: api,
		dfly:        dfly,
		cfg:         cfg,
		allowed:     allowed,
		allowAny:    allowAny,
	}, nil
}

// Run blocks until ctx is cancelled, processing Slack interactivity events. Intended to
// run as its own goroutine in the worker pool.
func (c *Controller) Run(ctx context.Context) error {
	handler := socketmode.NewSocketmodeHandler(c.smClient)
	handler.Handle(socketmode.EventTypeInteractive, c.dispatch)
	slog.Info("slackctl: starting Socket Mode controller", slog.Int("authorized_users", len(c.allowed)))
	if err := handler.RunEventLoopContext(ctx); err != nil {
		return fmt.Errorf("socketmode event loop: %w", err)
	}
	return nil
}

// dispatch fans block_actions out to the typed handlers. Other interactivity types
// (modal submits, view closes, etc.) are ack'd and ignored.
func (c *Controller) dispatch(evt *socketmode.Event, client *socketmode.Client) {
	// Always ack the request promptly so Slack doesn't retry. The handlers below run
	// asynchronously relative to the ack and surface errors via Slack messages, not via
	// the Socket Mode response.
	if evt.Request != nil {
		client.Ack(*evt.Request)
	}

	payload, ok := evt.Data.(slack.InteractionCallback)
	if !ok {
		slog.Warn("slackctl: dropping non-interactive event", slog.String("type", string(evt.Type)))
		return
	}
	if payload.Type != slack.InteractionTypeBlockActions {
		// Modals, shortcuts, etc. — not in scope for this feature.
		return
	}

	if !c.isAuthorized(payload.User.ID) {
		c.respondNotAuthorized(payload)
		return
	}

	// A single click usually carries one action, but the API allows for several. Process
	// them in the order Slack sent them so log ordering matches user intent.
	ctx := context.Background()
	for _, action := range payload.ActionCallback.BlockActions {
		switch action.ActionID {
		case transcribe.ActionIDRescueCancel:
			c.handleCancel(ctx, payload, action)
		case transcribe.ActionIDRescueClose:
			c.handleClose(ctx, payload, action)
		case transcribe.ActionIDRescueExtend:
			c.handleExtend(ctx, payload, action)
		case transcribe.ActionIDRescueSwitchTAC:
			c.handleSwitchTAC(ctx, payload, action)
		case transcribe.ActionIDFeedbackForm:
			// URL buttons fire a block_actions event AND open the link client-side — Slack
			// sends both. We have nothing to do server-side; this case exists only to
			// suppress the "unknown action_id" warn that would otherwise log on every click.
		default:
			slog.Warn("slackctl: unknown action_id", slog.String("action_id", action.ActionID))
		}
	}
}

func (c *Controller) isAuthorized(userID string) bool {
	if c.allowAny {
		return true
	}
	_, ok := c.allowed[userID]
	return ok
}

// respondNotAuthorized sends an ephemeral message visible only to the clicker. We don't
// surface "you are not in the allowlist" — that leaks the existence of an allowlist. A
// flatter "not permitted" message keeps the audit trail clean.
func (c *Controller) respondNotAuthorized(payload slack.InteractionCallback) {
	slog.Warn("slackctl: rejected unauthorized button press",
		slog.String("user", payload.User.ID),
		slog.String("user_name", payload.User.Name),
	)
	if payload.ResponseURL == "" {
		return
	}
	msg := slack.WebhookMessage{
		ResponseType:    "ephemeral",
		ReplaceOriginal: false,
		Text:            ":no_entry_sign: This control is restricted to incident leadership.",
	}
	if err := slack.PostWebhook(payload.ResponseURL, &msg); err != nil {
		slog.Error("slackctl: failed to send unauthorized response", slog.String("error", err.Error()))
	}
}

// postEphemeral sends a transient message via the click's response_url. Use this for
// per-click feedback ("cancelled successfully", "TAC has already expired", etc.) so other
// channel members aren't notified.
func (c *Controller) postEphemeral(payload slack.InteractionCallback, text string) {
	if payload.ResponseURL == "" {
		return
	}
	msg := slack.WebhookMessage{ResponseType: "ephemeral", Text: text}
	if err := slack.PostWebhook(payload.ResponseURL, &msg); err != nil {
		slog.Error("slackctl: failed to send ephemeral response", slog.String("error", err.Error()))
	}
}
