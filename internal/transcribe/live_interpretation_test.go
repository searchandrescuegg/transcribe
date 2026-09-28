package transcribe

import (
	"testing"
	"time"

	"github.com/searchandrescuegg/transcribe/internal/ml"
	"github.com/stretchr/testify/assert"
)

func TestBuildSummaryInput_SplitsKindsAndMarksVerified(t *testing.T) {
	meta := ClosureMeta{TACChannel: "TAC8", Transcription: "Rescue Trail TAC8",
		DispatchCorrection: &TranscriptCorrection{By: "U1", At: time.Now(), Original: "rescue tail"}}
	entries := []liveTranscriptEntry{
		{CapturedAt: "14:00:00", Text: "legacy radio"}, // pre-feature entry
		{CapturedAt: "14:01:00", Text: "5 year old female", Kind: entryKindRadio,
			Corrected: "54 year old female", Correction: &TranscriptCorrection{By: "U1"}},
		{Kind: entryKindInvalid},
		{CapturedAt: "14:02:00", Text: "diabetic per family", Kind: entryKindOperator},
		{CapturedAt: "14:03:00", Text: "retracted", Kind: entryKindOperator, Deleted: true},
	}
	prev := &ml.RescueSummary{Headline: "h"}

	in := buildSummaryInput(meta, entries, prev, "units")

	assert.True(t, in.DispatchVerified)
	assert.Equal(t, "Rescue - Trail", in.DispatchCallType)
	assert.Same(t, prev, in.PreviousSummary)
	assert.Equal(t, "units", in.UnitContext)
	assert.Equal(t, []ml.TACTranscript{
		{CapturedAt: "14:00:00", Text: "legacy radio"},
		{CapturedAt: "14:01:00", Text: "54 year old female", Verified: true},
	}, in.TACTranscripts)
	assert.Equal(t, []ml.OperatorCorrection{{At: "14:02:00", Text: "diabetic per family"}}, in.OperatorCorrections)
}
