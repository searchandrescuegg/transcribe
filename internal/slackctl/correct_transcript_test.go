package slackctl

import (
	"testing"

	"github.com/searchandrescuegg/transcribe/internal/transcribe"
	"github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCorrectionModal_RoundTrip(t *testing.T) {
	target := transcribe.CorrectionTarget{TGID: "1967", Kind: transcribe.CorrectionKindTAC,
		MessageTS: "111.1", ThreadTS: "100.0", CurrentText: "5 year old female"}
	modal, err := buildCorrectionModal(target)
	require.NoError(t, err)
	assert.Equal(t, CallbackIDCorrectionModal, modal.CallbackID)
	assert.LessOrEqual(t, len(modal.Title.Text), 24, "Slack modal titles max 24 chars")

	input := modal.Blocks.BlockSet[len(modal.Blocks.BlockSet)-1].(*slack.InputBlock)
	el := input.Element.(*slack.PlainTextInputBlockElement)
	assert.Equal(t, "5 year old female", el.InitialValue)
	assert.True(t, el.Multiline)

	// Simulate Slack echoing the view back on submit.
	view := slack.View{
		CallbackID:      modal.CallbackID,
		PrivateMetadata: modal.PrivateMetadata,
		State: &slack.ViewState{Values: map[string]map[string]slack.BlockAction{
			correctionInputBlockID: {correctionInputActionID: {Value: "54 year old female"}},
		}},
	}
	meta, text, err := parseCorrectionSubmission(view)
	require.NoError(t, err)
	assert.Equal(t, correctionModalMeta{MessageTS: "111.1", ThreadTS: "100.0"}, meta)
	assert.Equal(t, "54 year old female", text)
}

func TestParseCorrectionSubmission_BadMetadata(t *testing.T) {
	_, _, err := parseCorrectionSubmission(slack.View{PrivateMetadata: "{"})
	assert.Error(t, err)
}
