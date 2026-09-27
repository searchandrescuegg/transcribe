package transcribe

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func blocksJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

func TestCorrectionLabel_EscapesOneLinesAndTruncates(t *testing.T) {
	c := &TranscriptCorrection{By: "U123", At: time.Date(2026, 9, 27, 15, 4, 0, 0, time.Local),
		Original: "a <b> & ~c~\nsecond line " + strings.Repeat("x", 600)}
	out := blocksJSON(t, buildCorrectionContextBlock(c))

	// json.Marshal escapes < > & as < > &.
	assert.Contains(t, out, "Corrected by \\u003c@U123\\u003e · 15:04")
	assert.Contains(t, out, "\\u0026lt;b\\u0026gt; \\u0026amp;", "mrkdwn control chars escaped")
	assert.NotContains(t, out, "~c~", "tildes inside the struck text are neutralized")
	assert.NotContains(t, out, `\n`, "original collapsed to one line")
	assert.Contains(t, out, "…", "long original truncated")
}

func TestCorrectionLabel_OnlyWhenCorrected(t *testing.T) {
	in := &ThreadCommunicationBlocksInput{Channel: "TAC10", Message: "on scene", TS: time.Now()}
	plain := BuildThreadCommunicationBlocks(in)
	in.Correction = &TranscriptCorrection{By: "U1", At: time.Now(), Original: "on seen"}
	corrected := BuildThreadCommunicationBlocks(in)
	assert.Len(t, corrected, len(plain)+1)
	assert.Contains(t, blocksJSON(t, corrected), "Corrected by")

	rt := &RescueTrailBlocksInput{TACChannel: "TAC10", TranscriptionText: "t", ExpiresAt: time.Now(), DispatchTGID: "1399"}
	plainAlert := BuildRescueTrailBlocks(rt)
	rt.Correction = &TranscriptCorrection{By: "U1", At: time.Now(), Original: "o"}
	alert := BuildRescueTrailBlocks(rt)
	assert.Len(t, alert, len(plainAlert)+1)
	// Label sits directly after the preformatted transcription (index 3 → label at 4).
	assert.Contains(t, blocksJSON(t, alert[4]), "Corrected by")
}
