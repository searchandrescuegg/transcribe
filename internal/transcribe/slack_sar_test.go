package transcribe_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/searchandrescuegg/transcribe/internal/ml"
	"github.com/searchandrescuegg/transcribe/internal/transcribe"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// json.Marshal HTML-escapes "&" to "&", so assert on a substring free of special
// characters rather than the literal "Search & Rescue notified".
const sarBadgeText = "Rescue notified"

func marshalBlocks(t *testing.T, blocks any) string {
	t.Helper()
	raw, err := json.Marshal(blocks)
	require.NoError(t, err)
	return string(raw)
}

func TestBuildRescueTrailBlocks_SARNotifiedBadge(t *testing.T) {
	base := transcribe.RescueTrailBlocksInput{
		TACChannel:        "TAC3",
		TranscriptionText: "Rescue Trail TAC 3, Mount Si trailhead",
		ExpiresAt:         time.Date(2026, 7, 9, 10, 10, 0, 0, time.UTC),
		DispatchTGID:      transcribe.FireDispatch1TGID,
	}

	t.Run("badge present when notified", func(t *testing.T) {
		in := base
		in.SARNotified = true
		got := marshalBlocks(t, transcribe.BuildRescueTrailBlocks(&in))
		assert.Contains(t, got, sarBadgeText)
		assert.Contains(t, got, ":white_check_mark:")
	})

	t.Run("badge absent when not notified", func(t *testing.T) {
		in := base // SARNotified defaults false
		got := marshalBlocks(t, transcribe.BuildRescueTrailBlocks(&in))
		assert.NotContains(t, got, sarBadgeText)
	})

	t.Run("badge survives onto the closed alert", func(t *testing.T) {
		closedAt := time.Date(2026, 7, 9, 11, 0, 0, 0, time.UTC)
		in := base
		in.SARNotified = true
		in.ClosedAt = &closedAt
		got := marshalBlocks(t, transcribe.BuildRescueTrailBlocks(&in))
		assert.Contains(t, got, sarBadgeText, "closed alert must keep the SAR badge")
	})
}

func TestBuildLiveInterpretationBlocks_SARNotifiedBadge(t *testing.T) {
	updated := time.Date(2026, 7, 9, 10, 15, 0, 0, time.UTC)

	notified := marshalBlocks(t, transcribe.BuildLiveInterpretationBlocks(
		&ml.RescueSummary{Headline: "hiker down", SARNotified: true}, updated))
	assert.Contains(t, notified, sarBadgeText)

	quiet := marshalBlocks(t, transcribe.BuildLiveInterpretationBlocks(
		&ml.RescueSummary{Headline: "hiker down", SARNotified: false}, updated))
	assert.False(t, strings.Contains(quiet, sarBadgeText), "no badge when SAR not notified")
}
