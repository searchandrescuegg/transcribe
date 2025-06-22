package transcribe_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/searchandrescuegg/transcribe/internal/transcribe"
	"github.com/stretchr/testify/assert"
)

func TestBuildRescueTrailBlocks(t *testing.T) {

	loc, err := time.LoadLocation("America/Los_Angeles")
	assert.NoError(t, err, "should load PDT timezone")
	time.Local = loc

	tacChannel := "TAC3"
	transcriptionText := "Battalion 181 Engine 1838 171 26700 Southeast Issaqua Fall City Road Rescue Trail TAC 3 Battalion 181 Engine 1838 171 26700 Southeast Issaqua Fall City Road 16 27 hours"
	expiresAt := time.Date(2025, 6, 21, 10, 10, 0, 0, time.Local)

	blocks := transcribe.BuildRescueTrailBlocks(&transcribe.RescueTrailBlocksInput{
		TACChannel:        tacChannel,
		TranscriptionText: transcriptionText,
		ExpiresAt:         expiresAt,
	})

	jsonBlocks, err := json.Marshal(blocks)
	assert.NoError(t, err, "should marshal blocks to JSON")
	assert.NotEmpty(t, jsonBlocks, "blocks should not be empty")

	println(string(jsonBlocks)) // For debugging purposes

	assert.JSONEq(t, `[
		{
			"type": "header",
			"text": {
				"type": "plain_text",
				"text": "Rescue Trail :helmet_with_white_cross: :evergreen_tree: :mountain:",
				"emoji": true
			}
		},
		{
			"type": "divider"
		},
		{
			"type": "rich_text",
			"elements": [
				{
					"type": "rich_text_section",
					"elements": [
						{
							"type": "text",
							"text": "Channel: ",
							"style": {
								"bold": true
							}
						},
						{
							"type": "text",
							"text": "Fire Dispatch 1"
						}
					]
				}
			]
		},
		{
			"type": "rich_text",
			"elements": [
				{
					"type": "rich_text_preformatted",
					"elements": [
						{
							"type": "text",
							"text": "Battalion 181 Engine 1838 171 26700 Southeast Issaqua Fall City Road Rescue Trail TAC 3 Battalion 181 Engine 1838 171 26700 Southeast Issaqua Fall City Road 16 27 hours"
						}
					],
					"border": 0
				}
			]
		},
		{
			"type": "section",
			"text": {
				"type": "mrkdwn",
				"text": "Listen live on OpenMHz:"
			},
			"accessory": {
				"type": "button",
				"text": {
					"type": "plain_text",
					"text": ":headphones: Live Audio",
					"emoji": true
				},
				"action_id": "live-audio-button",
				"url": "https://openmhz.com/system/psernlfp?filter-type=talkgroup&filter-code=1385,1399"
			}
		},
		{
			"type": "divider"
		},
		{
			"type": "rich_text",
			"elements": [
				{
					"type": "rich_text_section",
					"elements": [
						{
							"type": "text",
							"text": "FTAC 3 transcription has been activated. "
						},
						{
							"type": "text",
							"text": "Expires 06/21/25 10:10 PDT",
							"style": {
								"bold": true,
								"italic": true
							}
						}
					]
				}
			]
		}
	]`, string(jsonBlocks), "should match expected JSON structure")

}
