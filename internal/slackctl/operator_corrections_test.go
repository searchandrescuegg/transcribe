package slackctl

import (
	"encoding/json"
	"testing"

	"github.com/slack-go/slack/slackevents"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func msgEvent(t *testing.T, raw string) *slackevents.MessageEvent {
	t.Helper()
	var ev slackevents.MessageEvent
	require.NoError(t, json.Unmarshal([]byte(raw), &ev))
	return &ev
}

const testBotID = "UBOT"

func TestExtractCorrection(t *testing.T) {
	for in, want := range map[string]string{
		"<@UBOT> broken ankle 54yo F":      "broken ankle 54yo F",
		"  <@UBOT>:  multi\nline ":         "multi\nline",
		"<@UBOT|pse rn> legacy label form": "legacy label form",
		"<@UBOT>: 54F not 5F":              "54F not 5F",
	} {
		got, ok := extractCorrection(in, testBotID)
		assert.True(t, ok, in)
		assert.Equal(t, want, got, in)
	}
	for _, in := range []string{
		"lol <@UBOT> is wrong again", // mid-sentence mention: not an opt-in
		"<@UBOT>   ",                 // mention with nothing to say
		"<@UOTHER> 54F",              // a different user
		"<@UBOTX> 54F",               // ID prefix collision
		"correction: 54F",            // the old prefix no longer counts
		"no mention",
	} {
		_, ok := extractCorrection(in, testBotID)
		assert.False(t, ok, in)
	}
	_, ok := extractCorrection("<@UBOT> 54F", "")
	assert.False(t, ok, "unknown bot ID disables mention corrections")
}

func TestParseCorrectionEvent(t *testing.T) {
	const ch = "C1"
	cases := []struct {
		name string
		raw  string
		ok   bool
		want correctionOp
	}{
		{"new thread reply", `{"type":"message","channel":"C1","user":"U1","text":"<@UBOT> 54yo F","ts":"200.1","thread_ts":"100.0"}`,
			true, correctionOp{Kind: correctionOpUpsert, ThreadTS: "100.0", MessageTS: "200.1", UserID: "U1", Text: "54yo F"}},
		{"thread broadcast", `{"type":"message","subtype":"thread_broadcast","channel":"C1","user":"U1","text":"<@UBOT> x","ts":"200.1","thread_ts":"100.0"}`,
			true, correctionOp{Kind: correctionOpUpsert, ThreadTS: "100.0", MessageTS: "200.1", UserID: "U1", Text: "x"}},
		{"old prefix ignored", `{"type":"message","channel":"C1","user":"U1","text":"correction: 54yo F","ts":"200.1","thread_ts":"100.0"}`, false, correctionOp{}},
		{"joke, no prefix", `{"type":"message","channel":"C1","user":"U1","text":"lol","ts":"200.1","thread_ts":"100.0"}`, false, correctionOp{}},
		{"top-level, not in thread", `{"type":"message","channel":"C1","user":"U1","text":"<@UBOT> x","ts":"200.1"}`, false, correctionOp{}},
		{"thread parent itself", `{"type":"message","channel":"C1","user":"U1","text":"<@UBOT> x","ts":"100.0","thread_ts":"100.0"}`, false, correctionOp{}},
		{"other channel", `{"type":"message","channel":"C2","user":"U1","text":"<@UBOT> x","ts":"200.1","thread_ts":"100.0"}`, false, correctionOp{}},
		{"bot", `{"type":"message","channel":"C1","bot_id":"B1","text":"<@UBOT> x","ts":"200.1","thread_ts":"100.0"}`, false, correctionOp{}},
		{"edit keeps prefix", `{"type":"message","subtype":"message_changed","channel":"C1",
			"message":{"user":"U1","text":"<@UBOT> 54yo F","ts":"200.1","thread_ts":"100.0"},
			"previous_message":{"user":"U1","text":"<@UBOT> 5yo F","ts":"200.1","thread_ts":"100.0"}}`,
			true, correctionOp{Kind: correctionOpUpsert, ThreadTS: "100.0", MessageTS: "200.1", UserID: "U1", Text: "54yo F"}},
		{"edit removes mention", `{"type":"message","subtype":"message_changed","channel":"C1",
			"message":{"user":"U1","text":"never mind","ts":"200.1","thread_ts":"100.0"},
			"previous_message":{"user":"U1","text":"<@UBOT> 5yo F","ts":"200.1","thread_ts":"100.0"}}`,
			true, correctionOp{Kind: correctionOpRemove, ThreadTS: "100.0", MessageTS: "200.1", UserID: "U1"}},
		{"edit of non-correction", `{"type":"message","subtype":"message_changed","channel":"C1",
			"message":{"user":"U1","text":"still a joke","ts":"200.1","thread_ts":"100.0"},
			"previous_message":{"user":"U1","text":"a joke","ts":"200.1","thread_ts":"100.0"}}`, false, correctionOp{}},
		{"deleted correction", `{"type":"message","subtype":"message_deleted","channel":"C1","deleted_ts":"200.1",
			"previous_message":{"user":"U1","text":"<@UBOT> 5yo F","ts":"200.1","thread_ts":"100.0"}}`,
			true, correctionOp{Kind: correctionOpRemove, ThreadTS: "100.0", MessageTS: "200.1", UserID: "U1"}},
		{"deleted joke", `{"type":"message","subtype":"message_deleted","channel":"C1","deleted_ts":"200.1",
			"previous_message":{"user":"U1","text":"lol","ts":"200.1","thread_ts":"100.0"}}`, false, correctionOp{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseCorrectionEvent(msgEvent(t, tc.raw), ch, testBotID)
			assert.Equal(t, tc.ok, ok)
			if tc.ok {
				assert.Equal(t, tc.want, got)
			}
		})
	}
}
