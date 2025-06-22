package transcribe

import (
	"fmt"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

// TODO: Build out variables for each piece of text in a struct ideally
//       Compute the url from openmhz based upon dispatch and the tactical channel that is activated
//       Example: https://openmhz.com/system/psernlfp?filter-type=talkgroup&filter-code=1389,1399 where 1399 is dispatch and 1389 is the tactical channel
//       Create separate block builder for the tactical channel messages
//       Create block builder when channel is closed? - time.AfterFunc looks good
//	     Leverage the thread_ts request parameter to send tactical messages to the same thread

type RescueTrailBlocksInput struct {
	TACChannel        string
	TranscriptionText string
	ExpiresAt         time.Time
}

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

	openMHzURL := buildOpenMHzURL([]string{talkgroup.TGID, "1399"}) // 1399 is fire dispatch 1

	blocks := []slack.Block{
		// Header block
		slack.NewHeaderBlock(
			slack.NewTextBlockObject(slack.PlainTextType, "Rescue Trail :helmet_with_white_cross: :evergreen_tree: :mountain:", true, false),
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

		// Rich text block with expiration info
		slack.NewRichTextBlock(
			"",
			slack.NewRichTextSection(
				slack.NewRichTextSectionTextElement(fmt.Sprintf("%s transcription has been activated. ", talkgroup.ShortName), nil),
				slack.NewRichTextSectionTextElement(
					fmt.Sprintf("Expires %s", rtbi.ExpiresAt.Format("01/02/06 15:04 MST")),
					&slack.RichTextSectionTextStyle{Bold: true, Italic: true},
				),
			),
		),
	}

	return blocks
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
