package prompts

import (
	"strings"
	"testing"

	"github.com/searchandrescuegg/transcribe/internal/ml"
	"github.com/stretchr/testify/assert"
)

// The King County gazetteer must be concatenated into both system prompts (in every
// dispatch branch and the summarizer), or local-location correction silently stops working.
func TestGazetteerAppendedToBothPrompts(t *testing.T) {
	// A canonical name and the delimiter header both distinguish the gazetteer from the
	// base prompt text.
	const marker = "Local place-name reference"
	const sampleName = "Mount Si"

	t.Run("dispatch with call-types", func(t *testing.T) {
		p := DispatchSystemPrompt([]string{"Rescue - Trail"})
		assert.Contains(t, p, marker)
		assert.Contains(t, p, sampleName)
		assert.Contains(t, p, "Snoqualmie")
	})

	t.Run("dispatch without call-types", func(t *testing.T) {
		p := DispatchSystemPrompt(nil)
		assert.Contains(t, p, marker)
		assert.Contains(t, p, sampleName)
	})

	t.Run("rescue summary", func(t *testing.T) {
		assert.Contains(t, RescueSummarySystemPrompt, marker)
		assert.Contains(t, RescueSummarySystemPrompt, sampleName)
		// Base instruction must still be present (gazetteer is appended, not replacing).
		assert.Contains(t, RescueSummarySystemPrompt, "emergency-response analyst")
	})
}

// The gazetteer instruction must keep its conservative "don't force a match" guardrail,
// which is what prevents the model from rewriting correctly-heard but unlisted locations.
func TestGazetteerHasConservativeFraming(t *testing.T) {
	assert.True(t, strings.Contains(kingCountyGazetteer, "do NOT force a match"),
		"gazetteer must retain the anti-over-correction instruction")
}

// Regression: a stronger model once hallucinated "Rattlesnake Ledge Trail" from
// "Norwell Hill Trail". The gazetteer MUST carry the hardened location-safety rule that forbids
// substituting one real place for a different-sounding one, and the cleanup prompt must reference
// it. This is a prompt-content guard (behavior can't be asserted deterministically without a live
// model), but it stops the guardrail from being silently dropped in an edit.
func TestGazetteerHasLocationSafetyGuardrail(t *testing.T) {
	assert.Contains(t, kingCountyGazetteer, "OBVIOUS PHONETIC NEAR-MATCH",
		"gazetteer must require an obvious phonetic near-match before correcting a location")
	assert.Contains(t, kingCountyGazetteer, "NEVER replace a place name with a DIFFERENT real place",
		"gazetteer must forbid substituting one real place for another")
	assert.Contains(t, kingCountyGazetteer, "misdirects emergency responders",
		"gazetteer must state the safety stakes so the model weights faithfulness over helpfulness")

	// The cleanup prompt must point the model at that safety rule.
	assert.Contains(t, TACCleanupSystemPrompt, "location safety rule")
	assert.Contains(t, TACCleanupSystemPrompt, "faithful garble is better than a confident fabrication")
}

// Regression: the model reinterpreted "Cadres provided" as "Grid reference provided" and
// "Cremio" as "primary" — semantic guesses that don't sound like the transcribed word. The
// cleanup prompt must require corrections to be phonetic near-matches, not domain-plausible
// substitutions.
func TestTACCleanupPromptForbidsSemanticSubstitution(t *testing.T) {
	assert.Contains(t, TACCleanupSystemPrompt, "obvious PHONETIC or spelling fix",
		"cleanup must restrict corrections to phonetic/spelling fixes")
	assert.Contains(t, TACCleanupSystemPrompt, "domain-plausible word from context is a fabrication")
}

// A trailing "Time X" is an elapsed call timer, not time-of-day; both the cleanup and summary
// prompts must say so, or the model renders it as a 24h clock (e.g. "1342") or slots it into the
// timeline as an event timestamp.
func TestPromptsTreatTrailingTimeAsElapsedCallTimer(t *testing.T) {
	assert.Contains(t, TACCleanupSystemPrompt, "ELAPSED call timer")
	assert.Contains(t, RescueSummarySystemPrompt, "ELAPSED call timer")
	assert.Contains(t, RescueSummarySystemPrompt, "KeyEvents timestamp",
		"summary must keep the call timer out of the KeyEvents timeline")
}

// The summarizer prompt must instruct the model on the SARNotified field, including the
// requirement that SAR be explicitly referenced (not inferred from a generic rescue).
func TestSummaryPromptCoversSARNotification(t *testing.T) {
	assert.Contains(t, RescueSummarySystemPrompt, "SARNotified")
	assert.Contains(t, RescueSummarySystemPrompt, "Search and Rescue")
}

// The additive behavior hinges on the prompt telling the model to preserve prior key events and
// extend rather than rewrite. If this instruction is dropped, the summary reverts to churning.
func TestSummaryPromptIsAdditive(t *testing.T) {
	assert.Contains(t, RescueSummarySystemPrompt, "PREVIOUS SUMMARY")
	assert.Contains(t, RescueSummarySystemPrompt, "preserve every prior KeyEvent")
}

// The previous-summary block is rendered only when a prior summary is supplied — a first pass
// must produce byte-identical output to the pre-feature builder (no stray header).
func TestBuildRescueSummaryUserPrompt_PreviousSummaryConditional(t *testing.T) {
	base := ml.RescueSummaryInput{
		DispatchTranscription: "Rescue Trail TAC8 Mount Si",
		DispatchCallType:      "Rescue - Trail",
		TACChannel:            "TAC8",
		TACTranscripts:        []ml.TACTranscript{{CapturedAt: "14:02", Text: "on scene"}},
	}
	first := BuildRescueSummaryUserPrompt(base)
	assert.NotContains(t, first, "PREVIOUS SUMMARY", "no prior-summary header on the first pass")

	withPrev := base
	withPrev.PreviousSummary = &ml.RescueSummary{
		Headline:  "Hiker with leg injury on Mount Si",
		KeyEvents: []ml.RescueSummaryEvent{{CapturedAt: "14:02", Description: "Battalion 171 on scene"}},
	}
	second := BuildRescueSummaryUserPrompt(withPrev)
	assert.Contains(t, second, "PREVIOUS SUMMARY")
	assert.Contains(t, second, "Battalion 171 on scene", "prior key events are fed back verbatim")
}

// The unit-context block is passed through into the user prompt when present.
func TestBuildRescueSummaryUserPrompt_UnitContext(t *testing.T) {
	in := ml.RescueSummaryInput{
		DispatchTranscription: "Rescue Trail",
		UnitContext:           "=== Units currently assigned to this call (from CAD) ===\n  - B171 (On Scene)",
	}
	assert.Contains(t, BuildRescueSummaryUserPrompt(in), "B171 (On Scene)")
}

// The cleanup prompt must keep the gazetteer (so place names get corrected) and its strict
// "do not invent" guardrail (so cleanup stays faithful), and the user builder must include the
// raw transmission plus any provided context.
func TestTACCleanupPrompt(t *testing.T) {
	assert.Contains(t, TACCleanupSystemPrompt, "Local place-name reference", "gazetteer appended")
	assert.Contains(t, TACCleanupSystemPrompt, "KCSO")
	assert.Contains(t, TACCleanupSystemPrompt, "Do NOT add information", "faithfulness guardrail")

	user := BuildTACCleanupUserPrompt(ml.TACCleanupInput{
		Text:            "tac two norwell hill trail",
		DispatchContext: "Rescue Trail TAC2",
		UnitContext:     "=== Units currently assigned to this call (from CAD) ===\n  - A181",
	})
	assert.Contains(t, user, "tac two norwell hill trail", "raw transmission included")
	assert.Contains(t, user, "Rescue Trail TAC2", "incident context included")
	assert.Contains(t, user, "A181", "unit context included")
}
