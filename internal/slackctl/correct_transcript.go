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
	"github.com/slack-go/slack/socketmode"
)

const (
	// CallbackIDCorrectTranscript is the message shortcut's callback_id in slack/manifest.yaml.
	CallbackIDCorrectTranscript = "correct_transcript"
	// CallbackIDCorrectionModal identifies our modal's view_submission.
	CallbackIDCorrectionModal = "correct_transcript_modal"

	correctionInputBlockID  = "correction_block"
	correctionInputActionID = "correction_text"
	maxCorrectionLength     = 3000 // Slack plain_text_input max
)

// correctionModalMeta rides in the modal's private_metadata so the submission can re-resolve
// (and re-check one-shot/active state) without trusting anything else in the payload.
type correctionModalMeta struct {
	MessageTS string `json:"message_ts"`
	ThreadTS  string `json:"thread_ts"`
}

func buildCorrectionModal(target transcribe.CorrectionTarget) (slack.ModalViewRequest, error) {
	meta, err := json.Marshal(correctionModalMeta{MessageTS: target.MessageTS, ThreadTS: target.ThreadTS})
	if err != nil {
		return slack.ModalViewRequest{}, err
	}
	what := "radio transmission"
	if target.Kind == transcribe.CorrectionKindDispatch {
		what = "dispatch transcript"
	}
	input := slack.NewPlainTextInputBlockElement(nil, correctionInputActionID).
		WithInitialValue(target.CurrentText).
		WithMultiline(true).
		WithMaxLength(maxCorrectionLength)
	return slack.ModalViewRequest{
		Type:            slack.VTModal,
		CallbackID:      CallbackIDCorrectionModal,
		PrivateMetadata: string(meta),
		Title:           slack.NewTextBlockObject(slack.PlainTextType, "Correct transcript", false, false),
		Submit:          slack.NewTextBlockObject(slack.PlainTextType, "Save correction", false, false),
		Close:           slack.NewTextBlockObject(slack.PlainTextType, "Cancel", false, false),
		Blocks: slack.Blocks{BlockSet: []slack.Block{
			slack.NewContextBlock("", slack.NewTextBlockObject(slack.MarkdownType,
				fmt.Sprintf("Fix what was actually said in this %s. *Only one correction is allowed per message*, and it will be labelled with your name.", what),
				false, false)),
			slack.NewInputBlock(correctionInputBlockID,
				slack.NewTextBlockObject(slack.PlainTextType, "Corrected text", false, false), nil, input),
		}},
	}, nil
}

func parseCorrectionSubmission(view slack.View) (correctionModalMeta, string, error) {
	var meta correctionModalMeta
	if err := json.Unmarshal([]byte(view.PrivateMetadata), &meta); err != nil {
		return correctionModalMeta{}, "", fmt.Errorf("private_metadata: %w", err)
	}
	if meta.MessageTS == "" {
		return correctionModalMeta{}, "", errors.New("private_metadata: missing message_ts")
	}
	text := ""
	if view.State != nil {
		text = view.State.Values[correctionInputBlockID][correctionInputActionID].Value
	}
	return meta, text, nil
}

// handleCorrectShortcut resolves the clicked message and opens the modal. Must finish within
// the trigger_id's 3s lifetime — resolution is a few Dragonfly reads.
func (c *Controller) handleCorrectShortcut(ctx context.Context, payload slack.InteractionCallback) {
	if c.corrections == nil {
		return
	}
	if payload.Channel.ID != c.cfg.SlackChannelID {
		c.postEphemeral(payload, correctionErrorMessage(transcribe.ErrNotCorrectable))
		return
	}
	msgTS := payload.Message.Timestamp
	if msgTS == "" {
		msgTS = payload.MessageTs
	}
	target, err := c.corrections.ResolveCorrectionTarget(ctx, msgTS, payload.Message.ThreadTimestamp)
	if err != nil {
		c.postEphemeral(payload, correctionErrorMessage(err))
		return
	}
	modal, err := buildCorrectionModal(target)
	if err != nil {
		slog.Error("slackctl: build correction modal", slog.String("error", err.Error()))
		return
	}
	if _, err := c.slackClient.OpenViewContext(ctx, payload.TriggerID, modal); err != nil {
		slog.Error("slackctl: open correction modal", slog.String("error", err.Error()))
		c.postEphemeral(payload, ":warning: Couldn't open the correction dialog; try again.")
	}
}

// handleCorrectionSubmission validates synchronously (errors render inline in the modal via the
// Ack payload), then acks to close the modal and applies asynchronously.
func (c *Controller) handleCorrectionSubmission(evt *socketmode.Event, client *socketmode.Client, payload slack.InteractionCallback) {
	fail := func(msg string) {
		client.Ack(*evt.Request, slack.NewErrorsViewSubmissionResponse(map[string]string{correctionInputBlockID: msg}))
	}
	if c.corrections == nil || !c.isAuthorized(payload.User.ID) {
		fail("Corrections are restricted to incident leadership.")
		return
	}
	meta, text, err := parseCorrectionSubmission(payload.View)
	if err != nil {
		slog.Warn("slackctl: bad correction submission", slog.String("error", err.Error()))
		fail(correctionErrorMessage(err))
		return
	}
	ctx, cancel := c.workCtx()
	target, err := c.corrections.ResolveCorrectionTarget(ctx, meta.MessageTS, meta.ThreadTS)
	if err == nil {
		_, err = transcribe.ValidateCorrectionText(target.CurrentText, text)
	}
	if err != nil {
		cancel()
		fail(correctionErrorMessage(err))
		return
	}
	client.Ack(*evt.Request)

	userID := payload.User.ID
	go func() {
		defer cancel()
		if err := c.corrections.ApplyTranscriptCorrection(ctx, target, userID, text, time.Now()); err != nil {
			slog.Warn("slackctl: apply correction failed", slog.String("error", err.Error()), slog.String("tgid", target.TGID))
			if _, perr := c.slackClient.PostEphemeralContext(ctx, c.cfg.SlackChannelID, userID,
				slack.MsgOptionText(correctionErrorMessage(err), false), slack.MsgOptionTS(target.ThreadTS)); perr != nil {
				slog.Error("slackctl: post correction failure ephemeral", slog.String("error", perr.Error()))
			}
			return
		}
		c.corrections.RefreshLiveInterpretation(ctx, target.TGID, true)
	}()
}
