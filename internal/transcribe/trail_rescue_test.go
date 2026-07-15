package transcribe

import (
	"testing"

	"github.com/searchandrescuegg/transcribe/internal/ml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTranscriptionSignalsTrailRescue(t *testing.T) {
	// The actual prod miss (2026-07-15): garbled opening, but the clean phrase appears.
	assert.True(t, transcriptionSignalsTrailRescue("Whoops Trail Tapu N131 Engine 16 Rescue Trail Tac 2 Battalion 131 133 Hours"))
	assert.True(t, transcriptionSignalsTrailRescue("Trail Rescue TAC 8 Mount Si trailhead"))
	// Must NOT fire on unrelated calls that merely mention a trail/rescue non-adjacently.
	assert.False(t, transcriptionSignalsTrailRescue("Aid Emergency TAC 1 on Trail Road"))
	assert.False(t, transcriptionSignalsTrailRescue("Rescue - General, water rescue at the lake"))
}

func TestTACChannelFromText(t *testing.T) {
	assert.Equal(t, "TAC2", tacChannelFromText("Rescue Trail Tac 2 Battalion 131"))
	assert.Equal(t, "TAC8", tacChannelFromText("rescue trail TAC8 Mount Si"))
	assert.Equal(t, "TAC10", tacChannelFromText("rescue trail tac 10"))
	assert.Equal(t, "", tacChannelFromText("no channel named here"))
	assert.Equal(t, "", tacChannelFromText("TAC 99"), "an out-of-range number is not a known TAC")
}

// The exact production failure: the LLM labeled a clearly-announced trail rescue as
// "Rescue - General" (dropping it). The safety net must recover it using the parsed TAC.
func TestSelectTrailRescueMessage_SafetyNet_RecoversMislabeled(t *testing.T) {
	msgs := dispatchMessages(ml.DispatchMessage{CallType: "Rescue - General", TACChannel: "TAC2", CleanedTranscription: "rescue trail"})
	got, hash := selectTrailRescueMessage(msgs, "Whoops Trail Tapu ... Rescue Trail Tac 2 Battalion 131 133 Hours")
	require.NotNil(t, got, "a clearly-announced trail rescue must not be dropped when the LLM mislabels the subtype")
	assert.Equal(t, "TAC2", got.TACChannel)
	assert.Equal(t, "Rescue - Trail", got.CallType, "salvaged message is normalized to the trail-rescue type")
	assert.NotEmpty(t, hash)
}

// When the LLM parsed no usable TAC, the net falls back to a regex on the transcription.
func TestSelectTrailRescueMessage_SafetyNet_RegexTACFallback(t *testing.T) {
	msgs := dispatchMessages(ml.DispatchMessage{CallType: "Rescue - General", TACChannel: ""})
	got, _ := selectTrailRescueMessage(msgs, "Rescue Trail TAC 8 Mount Si trailhead")
	require.NotNil(t, got)
	assert.Equal(t, "TAC8", got.TACChannel)
}

// No trail-rescue phrase in the transcription → nothing to salvage.
func TestSelectTrailRescueMessage_NoSignal_ReturnsNil(t *testing.T) {
	msgs := dispatchMessages(ml.DispatchMessage{CallType: "Aid - Emergency", TACChannel: "TAC1"})
	got, _ := selectTrailRescueMessage(msgs, "Aid Emergency TAC 1 at 112th Ave NE")
	assert.Nil(t, got, "no 'rescue trail' signal → no salvage")
}

// The happy path (LLM correctly classifies) must be unaffected — returns via the normal loop,
// never reaching the safety net.
func TestSelectTrailRescueMessage_NormalClassification_StillWorks(t *testing.T) {
	msgs := dispatchMessages(ml.DispatchMessage{CallType: "Rescue - Trail", TACChannel: "TAC8", CleanedTranscription: "clean"})
	got, _ := selectTrailRescueMessage(msgs, "Rescue Trail TAC 8")
	require.NotNil(t, got)
	assert.Equal(t, "TAC8", got.TACChannel)
	assert.Equal(t, "Rescue - Trail", got.CallType)
}
