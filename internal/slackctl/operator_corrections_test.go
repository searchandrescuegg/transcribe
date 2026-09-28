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

func TestStripCorrectionPrefix(t *testing.T) {
	for in, want := range map[string]string{
		"correction: broken ankle 54yo F": "broken ankle 54yo F",
		"  Correction :  multi\nline ":    "multi\nline",
		"CORRECTION:x":                    "x",
	} {
		got, ok := stripCorrectionPrefix(in)
		assert.True(t, ok, in)
		assert.Equal(t, want, got, in)
	}
	for _, in := range []string{"lol correction: nope", "correction:   ", "corrections: x", "no prefix"} {
		_, ok := stripCorrectionPrefix(in)
		assert.False(t, ok, in)
	}
}

func TestParseCorrectionEvent(t *testing.T) {
	const ch = "C1"
	cases := []struct {
		name string
		raw  string
		ok   bool
		want correctionOp
	}{
		{"new thread reply", `{"type":"message","channel":"C1","user":"U1","text":"correction: 54yo F","ts":"200.1","thread_ts":"100.0"}`,
			true, correctionOp{Kind: correctionOpUpsert, ThreadTS: "100.0", MessageTS: "200.1", UserID: "U1", Text: "54yo F"}},
		{"thread broadcast", `{"type":"message","subtype":"thread_broadcast","channel":"C1","user":"U1","text":"correction: x","ts":"200.1","thread_ts":"100.0"}`,
			true, correctionOp{Kind: correctionOpUpsert, ThreadTS: "100.0", MessageTS: "200.1", UserID: "U1", Text: "x"}},
		{"joke, no prefix", `{"type":"message","channel":"C1","user":"U1","text":"lol","ts":"200.1","thread_ts":"100.0"}`, false, correctionOp{}},
		{"top-level, not in thread", `{"type":"message","channel":"C1","user":"U1","text":"correction: x","ts":"200.1"}`, false, correctionOp{}},
		{"thread parent itself", `{"type":"message","channel":"C1","user":"U1","text":"correction: x","ts":"100.0","thread_ts":"100.0"}`, false, correctionOp{}},
		{"other channel", `{"type":"message","channel":"C2","user":"U1","text":"correction: x","ts":"200.1","thread_ts":"100.0"}`, false, correctionOp{}},
		{"bot", `{"type":"message","channel":"C1","bot_id":"B1","text":"correction: x","ts":"200.1","thread_ts":"100.0"}`, false, correctionOp{}},
		{"edit keeps prefix", `{"type":"message","subtype":"message_changed","channel":"C1",
			"message":{"user":"U1","text":"correction: 54yo F","ts":"200.1","thread_ts":"100.0"},
			"previous_message":{"user":"U1","text":"correction: 5yo F","ts":"200.1","thread_ts":"100.0"}}`,
			true, correctionOp{Kind: correctionOpUpsert, ThreadTS: "100.0", MessageTS: "200.1", UserID: "U1", Text: "54yo F"}},
		{"edit removes prefix", `{"type":"message","subtype":"message_changed","channel":"C1",
			"message":{"user":"U1","text":"never mind","ts":"200.1","thread_ts":"100.0"},
			"previous_message":{"user":"U1","text":"correction: 5yo F","ts":"200.1","thread_ts":"100.0"}}`,
			true, correctionOp{Kind: correctionOpRemove, ThreadTS: "100.0", MessageTS: "200.1", UserID: "U1"}},
		{"edit of non-correction", `{"type":"message","subtype":"message_changed","channel":"C1",
			"message":{"user":"U1","text":"still a joke","ts":"200.1","thread_ts":"100.0"},
			"previous_message":{"user":"U1","text":"a joke","ts":"200.1","thread_ts":"100.0"}}`, false, correctionOp{}},
		{"deleted correction", `{"type":"message","subtype":"message_deleted","channel":"C1","deleted_ts":"200.1",
			"previous_message":{"user":"U1","text":"correction: 5yo F","ts":"200.1","thread_ts":"100.0"}}`,
			true, correctionOp{Kind: correctionOpRemove, ThreadTS: "100.0", MessageTS: "200.1", UserID: "U1"}},
		{"deleted joke", `{"type":"message","subtype":"message_deleted","channel":"C1","deleted_ts":"200.1",
			"previous_message":{"user":"U1","text":"lol","ts":"200.1","thread_ts":"100.0"}}`, false, correctionOp{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseCorrectionEvent(msgEvent(t, tc.raw), ch)
			assert.Equal(t, tc.ok, ok)
			if tc.ok {
				assert.Equal(t, tc.want, got)
			}
		})
	}
}
