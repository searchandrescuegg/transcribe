package transcribe

import (
	"fmt"
	"strings"
	"time"

	"github.com/searchandrescuegg/transcribe/internal/ml"
	"github.com/slack-go/slack"
)

// TODO: Build out variables for each piece of text in a struct ideally
//       Compute the url from openmhz based upon dispatch and the tactical channel that is activated
//       Example: https://openmhz.com/system/psernlfp?filter-type=talkgroup&filter-code=1389,1399 where 1399 is dispatch and 1389 is the tactical channel
//       Create separate block builder for the tactical channel messages
//       Create block builder when channel is closed? - time.AfterFunc looks good
//	     Leverage the thread_ts request parameter to send tactical messages to the same thread

// FIX (review item #22): DispatchTGID is now passed in instead of being a hardcoded "1399"
// inside the builder. The block builder is no longer coupled to package state, which
// makes its tests independent of the talkgroups.go contents.
//
// TACTalkgroupTGID is the TGID of the activated tactical channel (e.g. "1389" for TAC1).
// It's encoded as the action `value` on the Cancel and Extend buttons so the Slack
// interactivity controller can identify which TAC to operate on without parsing the
// message text. Empty string disables the action buttons (used by older callers / tests).
//
// ClosedAt switches the builder into "closed" mode: the trailing "Expires HH:MM" line
// becomes "Monitoring auto-closed at HH:MM", the actions block is omitted (no buttons),
// and the header label gains a :lock: emoji. Used by the sweeper's chat.update path so
// the alert reflects the actual closed state in-place.
//
// FeedbackURL is the prefilled Google Form URL. Rendered as a button on closed alerts
// only — empty (or unset) means no button, which is the case when ClosedAt is nil
// (mid-rescue) or when FEEDBACK_FORM_URL isn't configured at the env level.
type RescueTrailBlocksInput struct {
	TACChannel        string
	TranscriptionText string
	ExpiresAt         time.Time
	DispatchTGID      string
	TACTalkgroupTGID  string
	ClosedAt          *time.Time
	FeedbackURL       string
}

// Action IDs are the routing keys the slackctl controller dispatches on. Keep these in
// sync with internal/slackctl/controller.go.
const (
	ActionIDRescueCancel = "rescue_cancel"
	// ActionIDRescueClose ends the rescue early but routes through the same path the
	// auto-expiry sweeper takes — distinct from Cancel (false-alarm), which wipes the
	// alert + summary context. Close preserves the structured Live Interpretation,
	// rewrites the parent alert with the Submit Feedback button, and posts a normal
	// "Channel Closed" reply, identical to a natural expiry.
	ActionIDRescueClose     = "rescue_close"
	ActionIDRescueExtend    = "rescue_extend"
	ActionIDRescueSwitchTAC = "rescue_switch_tac"
	// ActionIDFeedbackForm is the action_id on the URL-style Submit Feedback button.
	// Slack sends a block_actions event for URL buttons too (so engagement is trackable),
	// but we have nothing server-side to do — the controller's switch handles it as a
	// no-op, preventing a noisy "unknown action_id" warn on every click.
	ActionIDFeedbackForm = "feedback_form"
	// ActionsBlockIDPrefix is the literal portion of the actions block_id. The full id is
	// "rescue_actions:<TGID>", letting the switch-TAC handler recover the previous TGID
	// without needing to look it up in Dragonfly. Cancel/Extend continue to read the TGID
	// from the button's value field; only switch needs the block_id route because the
	// select element's value carries the NEW (target) TGID.
	ActionsBlockIDPrefix = "rescue_actions"
)

func buildOpenMHzURL(channels []string) string {
	return fmt.Sprintf("https://openmhz.com/system/psernlfp?filter-type=talkgroup&filter-code=%s", strings.Join(channels, ","))
}

// BuildRescueTrailBlocks creates Slack Block Kit blocks for rescue trail notifications
func BuildRescueTrailBlocks(rtbi *RescueTrailBlocksInput) []slack.Block {

	talkgroup, ok := talkgroupFromRadioShortCode[rtbi.TACChannel]
	if !ok {
		talkgroup = TalkgroupInformation{
			TGID:      "unknown",
			FullName:  "Unknown Channel",
			ShortName: "Unknown",
		}
	}

	openMHzURL := buildOpenMHzURL([]string{talkgroup.TGID, rtbi.DispatchTGID})

	headerText := "Rescue Trail :helmet_with_white_cross: :evergreen_tree: :mountain:"
	if rtbi.ClosedAt != nil {
		headerText = "Rescue Trail :helmet_with_white_cross: :lock:"
	}

	blocks := []slack.Block{
		// Header block
		slack.NewHeaderBlock(
			slack.NewTextBlockObject(slack.PlainTextType, headerText, true, false),
		),

		// Divider
		slack.NewDividerBlock(),

		// Rich text block with FTAC Channel info
		slack.NewRichTextBlock(
			"",
			slack.NewRichTextSection(
				slack.NewRichTextSectionTextElement("Channel: ", &slack.RichTextSectionTextStyle{Bold: true}),
				slack.NewRichTextSectionTextElement("Fire Dispatch 1", nil),
			),
		),

		// Rich text block with preformatted transcription
		slack.NewRichTextBlock(
			"",
			&slack.RichTextPreformatted{
				RichTextSection: slack.RichTextSection{
					Type: slack.RTEPreformatted,
					Elements: []slack.RichTextSectionElement{
						slack.NewRichTextSectionTextElement(rtbi.TranscriptionText, nil),
					},
				},
				Border: 0,
			},
		),

		// Section with button
		slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType, "Listen live on OpenMHz:", false, false),
			nil,
			slack.NewAccessory(
				slack.NewButtonBlockElement(
					"live-audio-button",
					"",
					slack.NewTextBlockObject(slack.PlainTextType, ":headphones: Live Audio", true, false),
				).WithURL(openMHzURL),
			),
		),

		// Divider
		slack.NewDividerBlock(),

		// Rich text block with expiration / closure info
		buildRescueStatusBlock(talkgroup.ShortName, rtbi.ExpiresAt, rtbi.ClosedAt),
	}

	// Action buttons only render while the rescue is live. Once ClosedAt is set the alert
	// is a frozen historical record — leadership can't extend or cancel a closed rescue.
	if rtbi.TACTalkgroupTGID != "" && rtbi.ClosedAt == nil {
		blocks = append(blocks, buildRescueActionsBlock(rtbi.TACChannel, rtbi.TACTalkgroupTGID))
	}

	// Feedback button only on closed alerts AND only when a form URL was configured.
	// It's a URL button (not an interactivity action), so no controller routing — Slack
	// just opens the link in the user's browser.
	if rtbi.ClosedAt != nil && rtbi.FeedbackURL != "" {
		blocks = append(blocks, buildFeedbackButtonBlock(rtbi.FeedbackURL))
	}

	return blocks
}

// buildFeedbackButtonBlock returns the section + button that links to the prefilled form.
// Action ID is non-empty so Slack accepts the block, but no slackctl handler is registered
// for it — clicks open the URL client-side without round-tripping through the bot.
func buildFeedbackButtonBlock(formURL string) slack.Block {
	btn := slack.NewButtonBlockElement(
		ActionIDFeedbackForm,
		"",
		slack.NewTextBlockObject(slack.PlainTextType, ":memo: Submit Feedback", true, false),
	).WithURL(formURL)
	return slack.NewSectionBlock(
		slack.NewTextBlockObject(slack.MarkdownType, "_Help us tune the model — share what was right or wrong about this alert._", false, false),
		nil,
		slack.NewAccessory(btn),
	)
}

// buildRescueStatusBlock renders either "FTAC X transcription has been activated. Expires …"
// (live) or "FTAC X monitoring auto-closed at …" (post-closure). Kept on a single block so
// chat.update is a clean swap of one element rather than juggling adjacent dividers.
func buildRescueStatusBlock(shortName string, expiresAt time.Time, closedAt *time.Time) slack.Block {
	if closedAt != nil {
		return slack.NewRichTextBlock(
			"",
			slack.NewRichTextSection(
				slack.NewRichTextSectionTextElement(fmt.Sprintf("%s monitoring auto-closed at ", shortName), nil),
				slack.NewRichTextSectionTextElement(
					closedAt.Format("01/02/06 15:04 MST"),
					&slack.RichTextSectionTextStyle{Bold: true, Italic: true},
				),
				slack.NewRichTextSectionTextElement(".", nil),
			),
		)
	}
	return slack.NewRichTextBlock(
		"",
		slack.NewRichTextSection(
			slack.NewRichTextSectionTextElement(fmt.Sprintf("%s transcription has been activated. ", shortName), nil),
			slack.NewRichTextSectionTextElement(
				fmt.Sprintf("Expires %s", expiresAt.Format("01/02/06 15:04 MST")),
				&slack.RichTextSectionTextStyle{Bold: true, Italic: true},
			),
		),
	)
}

// buildRescueActionsBlock renders the Cancel + Extend buttons and the Switch-TAC select.
// Cancel/Extend carry the current TGID as their button value; the select carries the
// target TGID per option, with the current TGID encoded in the action block's id so the
// switch handler can derive both old and new in one click.
func buildRescueActionsBlock(tacChannel, tacTGID string) slack.Block {
	cancelBtn := slack.NewButtonBlockElement(
		ActionIDRescueCancel,
		tacTGID,
		slack.NewTextBlockObject(slack.PlainTextType, "Cancel (False Alarm)", true, false),
	)
	cancelBtn.Style = slack.StyleDanger
	cancelBtn.Confirm = slack.NewConfirmationBlockObject(
		slack.NewTextBlockObject(slack.PlainTextType, fmt.Sprintf("Cancel %s monitoring?", tacChannel), false, false),
		slack.NewTextBlockObject(slack.PlainTextType, "This stops transcription on this tactical channel and posts a cancellation notice in the thread.", false, false),
		slack.NewTextBlockObject(slack.PlainTextType, "Cancel monitoring", false, false),
		slack.NewTextBlockObject(slack.PlainTextType, "Keep monitoring", false, false),
	)

	closeBtn := slack.NewButtonBlockElement(
		ActionIDRescueClose,
		tacTGID,
		slack.NewTextBlockObject(slack.PlainTextType, "Close (End Rescue)", true, false),
	)
	closeBtn.Confirm = slack.NewConfirmationBlockObject(
		slack.NewTextBlockObject(slack.PlainTextType, fmt.Sprintf("Close %s monitoring?", tacChannel), false, false),
		slack.NewTextBlockObject(slack.PlainTextType, "Ends the rescue early. The Live Interpretation summary, dispatch context, and Submit Feedback button are preserved — same as a natural auto-close.", false, false),
		slack.NewTextBlockObject(slack.PlainTextType, "Close monitoring", false, false),
		slack.NewTextBlockObject(slack.PlainTextType, "Keep monitoring", false, false),
	)

	extendBtn := slack.NewButtonBlockElement(
		ActionIDRescueExtend,
		tacTGID,
		slack.NewTextBlockObject(slack.PlainTextType, "Extend monitoring", true, false),
	)
	extendBtn.Confirm = slack.NewConfirmationBlockObject(
		slack.NewTextBlockObject(slack.PlainTextType, fmt.Sprintf("Extend %s monitoring?", tacChannel), false, false),
		slack.NewTextBlockObject(slack.PlainTextType, "This pushes the auto-close out by another full activation window.", false, false),
		slack.NewTextBlockObject(slack.PlainTextType, "Extend", false, false),
		slack.NewTextBlockObject(slack.PlainTextType, "Cancel", false, false),
	)

	switchSelect := buildSwitchTACSelect(tacChannel)

	// block_id encodes the active TGID so the switch handler knows what to migrate FROM.
	blockID := fmt.Sprintf("%s:%s", ActionsBlockIDPrefix, tacTGID)
	return slack.NewActionBlock(blockID, cancelBtn, closeBtn, extendBtn, switchSelect)
}

// buildSwitchTACSelect returns a static_select populated with TAC1-TAC10. Each option's
// value is the TARGET TGID; the source TGID lives in the parent block_id. Confirm dialog
// fires before the select event reaches our controller — destructive correction needs a
// fat-finger guard same as Cancel/Extend.
func buildSwitchTACSelect(currentTACChannel string) *slack.SelectBlockElement {
	// TAC1..TAC10 in stable, human-readable order. We read the canonical entries from the
	// derived short-code map so this stays in lockstep with talkgroups.go.
	codes := []string{"TAC1", "TAC2", "TAC3", "TAC4", "TAC5", "TAC6", "TAC7", "TAC8", "TAC9", "TAC10"}
	options := make([]*slack.OptionBlockObject, 0, len(codes))
	for _, code := range codes {
		tg, ok := talkgroupFromRadioShortCode[code]
		if !ok {
			continue
		}
		// Target TGID lives in option.value; the user-facing label is the short code plus
		// the human-friendly name so it's clear which channel they're picking.
		label := fmt.Sprintf("%s — %s", code, tg.ShortName)
		options = append(options, slack.NewOptionBlockObject(
			tg.TGID,
			slack.NewTextBlockObject(slack.PlainTextType, label, false, false),
			nil,
		))
	}

	placeholder := slack.NewTextBlockObject(slack.PlainTextType, fmt.Sprintf("Switch from %s…", currentTACChannel), false, false)
	sel := slack.NewOptionsSelectBlockElement(slack.OptTypeStatic, placeholder, ActionIDRescueSwitchTAC, options...)
	sel.Confirm = slack.NewConfirmationBlockObject(
		slack.NewTextBlockObject(slack.PlainTextType, "Switch TAC channel?", false, false),
		slack.NewTextBlockObject(slack.PlainTextType, fmt.Sprintf("This stops transcription on %s and starts it on the channel you pick. The activation window resets to a fresh full duration on the new TAC.", currentTACChannel), false, false),
		slack.NewTextBlockObject(slack.PlainTextType, "Switch", false, false),
		slack.NewTextBlockObject(slack.PlainTextType, "Keep current", false, false),
	)
	return sel
}

type ThreadCommunicationBlocksInput struct {
	Channel string
	Message string
	TS      time.Time
}

func BuildThreadCommunicationBlocks(tcbi *ThreadCommunicationBlocksInput) []slack.Block {
	blocks := []slack.Block{
		slack.NewRichTextBlock(
			"",
			slack.NewRichTextSection(
				slack.NewRichTextSectionTextElement("Time: ", &slack.RichTextSectionTextStyle{Bold: true}),
				slack.NewRichTextSectionTextElement(tcbi.TS.Local().Format(time.RFC1123), nil),
			),
			slack.NewRichTextSection(
				slack.NewRichTextSectionTextElement("Channel: ", &slack.RichTextSectionTextStyle{Bold: true}),
				slack.NewRichTextSectionTextElement(tcbi.Channel, nil),
			),
		),
		slack.NewRichTextBlock(
			"",
			&slack.RichTextPreformatted{
				RichTextSection: slack.RichTextSection{
					Type: slack.RTEPreformatted,
					Elements: []slack.RichTextSectionElement{
						slack.NewRichTextSectionTextElement(tcbi.Message, nil),
					},
				},
				Border: 0,
			},
		),
		slack.NewDividerBlock(),
	}

	return blocks
}

// BuildLiveInterpretationBlocks renders a structured rescue summary for the rolling
// "Live Interpretation" message in the rescue thread. Posted on the first TAC transmission
// and chat.update'd on each subsequent one. UpdatedAt is the moment the most recent TAC
// transmission was processed; it lets viewers see how fresh the summary is.
func BuildLiveInterpretationBlocks(s *ml.RescueSummary, updatedAt time.Time) []slack.Block {
	blocks := []slack.Block{
		slack.NewHeaderBlock(
			slack.NewTextBlockObject(slack.PlainTextType, "Live Interpretation :dna:", true, false),
		),
	}

	if s.Headline != "" {
		blocks = append(blocks,
			slack.NewSectionBlock(
				slack.NewTextBlockObject(slack.MarkdownType, "*"+s.Headline+"*", false, false),
				nil, nil,
			),
		)
	}

	if s.SituationSummary != "" {
		blocks = append(blocks,
			slack.NewSectionBlock(
				slack.NewTextBlockObject(slack.MarkdownType, s.SituationSummary, false, false),
				nil, nil,
			),
		)
	}

	// Field block — Slack section blocks accept a 2-column field array which renders nicely
	// for short label:value pairs. Only emit fields that have content so the message stays
	// dense.
	var fields []*slack.TextBlockObject
	if s.Location != "" {
		fields = append(fields,
			slack.NewTextBlockObject(slack.MarkdownType, "*Location*\n"+s.Location, false, false),
		)
	}
	if s.PatientStatus != "" {
		fields = append(fields,
			slack.NewTextBlockObject(slack.MarkdownType, "*Patient*\n"+s.PatientStatus, false, false),
		)
	}
	if s.Outcome != "" {
		fields = append(fields,
			slack.NewTextBlockObject(slack.MarkdownType, "*Outcome*\n"+s.Outcome, false, false),
		)
	}
	if len(s.UnitsInvolved) > 0 {
		fields = append(fields,
			slack.NewTextBlockObject(slack.MarkdownType, "*Units*\n"+strings.Join(s.UnitsInvolved, ", "), false, false),
		)
	}
	if len(fields) > 0 {
		blocks = append(blocks, slack.NewSectionBlock(nil, fields, nil))
	}

	if len(s.KeyEvents) > 0 {
		var b strings.Builder
		b.WriteString("*Key events*\n")
		for _, e := range s.KeyEvents {
			b.WriteString("• ")
			if e.CapturedAt != "" {
				b.WriteString("`")
				b.WriteString(e.CapturedAt)
				b.WriteString("` — ")
			}
			b.WriteString(e.Description)
			b.WriteString("\n")
		}
		blocks = append(blocks,
			slack.NewSectionBlock(
				slack.NewTextBlockObject(slack.MarkdownType, b.String(), false, false),
				nil, nil,
			),
		)
	}

	blocks = append(blocks,
		slack.NewContextBlock("",
			slack.NewTextBlockObject(slack.MarkdownType,
				fmt.Sprintf(":hourglass_flowing_sand: Updated %s — refreshes after each TAC transmission.",
					updatedAt.Format("01/02/06 15:04 MST")),
				false, false),
		),
	)

	return blocks
}

type ChannelClosedBlocksInput struct {
	Channel  string
	ClosedAt time.Time
}

func BuildChannelClosedBlocks(ccbi *ChannelClosedBlocksInput) []slack.Block {
	blocks := []slack.Block{
		slack.NewHeaderBlock(
			slack.NewTextBlockObject(slack.PlainTextType, "Channel Closed :lock:", true, false),
		),
		slack.NewDividerBlock(),
		slack.NewRichTextBlock(
			"",
			slack.NewRichTextSection(
				slack.NewRichTextSectionTextElement(
					fmt.Sprintf("Channel %s has been closed.", ccbi.Channel),
					&slack.RichTextSectionTextStyle{Bold: true},
				),
			),
		),
		slack.NewRichTextBlock(
			"",
			slack.NewRichTextSection(
				slack.NewRichTextSectionTextElement(
					fmt.Sprintf("Closed at %s", ccbi.ClosedAt.Local().Format(time.RFC1123)),
					&slack.RichTextSectionTextStyle{Bold: true},
				),
			),
		),
	}

	return blocks
}
