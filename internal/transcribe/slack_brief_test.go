package transcribe

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/searchandrescuegg/transcribe/internal/ml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFormatBrief(t *testing.T) {
	cases := []struct {
		name string
		in   *ml.RescueSummary
		want string
	}{
		{"nil", nil, ""},
		{"all empty", &ml.RescueSummary{}, ""},
		{"all slots", &ml.RescueSummary{BriefLocation: "Mailbox Peak", BriefSubject: "54F", BriefCondition: "ankle injury"}, "Mailbox Peak · 54F · ankle injury"},
		{"empty middle dropped", &ml.RescueSummary{BriefLocation: "Tiger Mtn", BriefCondition: "cardiac"}, "Tiger Mtn · cardiac"},
		{"whitespace collapsed", &ml.RescueSummary{BriefLocation: "  Mailbox\n  Peak ", BriefSubject: "   "}, "Mailbox Peak"},
		{"code and strike markers stripped", &ml.RescueSummary{BriefLocation: "`trail`", BriefCondition: "~ankle~ injury"}, "trail · ankle injury"},
		{"mrkdwn escaped and bold-breakers stripped", &ml.RescueSummary{BriefLocation: "A & B <x>", BriefCondition: "*bad*_leg_"}, "A &amp; B &lt;x&gt; · badleg"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { assert.Equal(t, tc.want, FormatBrief(tc.in)) })
	}

	long := FormatBrief(&ml.RescueSummary{BriefLocation: strings.Repeat("x", 60)})
	assert.Equal(t, strings.Repeat("x", 40)+"…", long, "each slot truncated to 40 runes")
}

func TestRescueTrailBlocks_Brief(t *testing.T) {
	base := func() *RescueTrailBlocksInput {
		return &RescueTrailBlocksInput{TACChannel: "TAC10", TranscriptionText: "t", ExpiresAt: time.Unix(1_700_000_000, 0), DispatchTGID: "1399", TACTalkgroupTGID: "1967"}
	}
	plain, err := json.Marshal(BuildRescueTrailBlocks(base()))
	require.NoError(t, err)
	withEmpty := base()
	withEmpty.Brief = ""
	same, err := json.Marshal(BuildRescueTrailBlocks(withEmpty))
	require.NoError(t, err)
	assert.Equal(t, string(plain), string(same), "empty brief must be byte-identical")

	all := base()
	all.Brief = "Mailbox Peak · 54F · ankle injury"
	all.SARNotified = true
	all.Correction = &TranscriptCorrection{By: "U1", At: time.Now(), Original: "o"}
	blocks := BuildRescueTrailBlocks(all)
	j := func(i int) string { b, _ := json.Marshal(blocks[i]); return string(b) }
	assert.Contains(t, j(1), "*Mailbox Peak · 54F · ankle injury*", "brief directly under header")
	assert.Contains(t, j(2), "Search \\u0026 Rescue notified", "SAR badge after brief")
	full, _ := json.Marshal(blocks)
	assert.Contains(t, string(full), "Corrected by")
}
