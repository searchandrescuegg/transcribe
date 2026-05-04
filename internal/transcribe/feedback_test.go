package transcribe

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/searchandrescuegg/transcribe/internal/config"
	"github.com/searchandrescuegg/transcribe/internal/dragonfly"
	"github.com/searchandrescuegg/transcribe/internal/ml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseFeedbackFieldMap(t *testing.T) {
	cases := map[string]struct {
		in    string
		want  map[string]string
		empty bool
	}{
		"empty input":           {in: "", empty: true},
		"whitespace only":       {in: "  ", empty: true},
		"valid mapping":         {in: `{"tac_channel":"entry.111","headline":"entry.222"}`, want: map[string]string{"tac_channel": "entry.111", "headline": "entry.222"}},
		"unknown logical names": {in: `{"weather":"entry.999"}`, want: map[string]string{"weather": "entry.999"}}, // parser doesn't validate keys
		"malformed JSON":        {in: `{not valid`, empty: true},
		"non-string values":     {in: `{"tac_channel":123}`, empty: true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := parseFeedbackFieldMap(c.in)
			if c.empty {
				assert.Nil(t, got)
				return
			}
			assert.Equal(t, c.want, got)
		})
	}
}

// buildFeedbackURL needs Dragonfly for the summary_data read. Reuses the suite's container
// via a lightweight helper that constructs a TranscribeClient with just the bits we need.
func (s *DispatchSuite) TestBuildFeedbackURL_NoConfig_ReturnsEmpty() {
	tc := s.newClientForFeedbackTests("", "")
	got := tc.buildFeedbackURL(s.ctx, "1967", ClosureMeta{TACChannel: "TAC10"}, time.Now())
	s.Empty(got, "no FEEDBACK_FORM_URL = no button = empty string")
}

func (s *DispatchSuite) TestBuildFeedbackURL_URLOnlyNoFields_ReturnsBareURL() {
	formURL := "https://docs.google.com/forms/d/e/abc/viewform"
	tc := s.newClientForFeedbackTests(formURL, "")
	got := tc.buildFeedbackURL(s.ctx, "1967", ClosureMeta{TACChannel: "TAC10"}, time.Now())
	s.Equal(formURL, got, "URL configured but no field map → bare URL, no prefill")
}

func (s *DispatchSuite) TestBuildFeedbackURL_FullPrefill_IncludesAllMappedFields() {
	formURL := "https://docs.google.com/forms/d/e/abc/viewform"
	fields := `{"tac_channel":"entry.111","closed_at":"entry.222","dispatch_transcript":"entry.333","headline":"entry.444","situation_summary":"entry.555"}`
	tc := s.newClientForFeedbackTests(formURL, fields)

	// Pre-populate summary_data so the helper has headline + situation_summary.
	summary := ml.RescueSummary{
		Headline:         "Bicycle accident on Tiger Mountain Trail",
		SituationSummary: "Engine 8171 dispatched; patient ambulatory.",
	}
	encoded, _ := json.Marshal(summary)
	s.Require().NoError(s.rdb.Set(s.ctx, fmt.Sprintf(summaryDataKeyFmt, "1967"), encoded, 1*time.Hour).Err())

	closedAt := time.Date(2026, 5, 3, 16, 36, 27, 0, time.Local)
	got := tc.buildFeedbackURL(s.ctx, "1967", ClosureMeta{
		TACChannel:    "TAC10",
		Transcription: "Rescue Trail TAC 10 ...",
	}, closedAt)

	// The URL must keep the base intact and append a single ?-prefixed query string.
	s.True(strings.HasPrefix(got, formURL+"?"), "URL must start with the base form URL plus ?")

	// Parse the query string and assert each entry is present + correctly URL-encoded.
	parsed, err := url.Parse(got)
	s.Require().NoError(err)
	q := parsed.Query()
	s.Equal("TAC10", q.Get("entry.111"))
	s.Equal("05/03/26 16:36 PDT", q.Get("entry.222"))
	s.Equal("Rescue Trail TAC 10 ...", q.Get("entry.333"))
	s.Equal("Bicycle accident on Tiger Mountain Trail", q.Get("entry.444"))
	s.Equal("Engine 8171 dispatched; patient ambulatory.", q.Get("entry.555"))
}

func (s *DispatchSuite) TestBuildFeedbackURL_NoSummaryData_FallsBackToDispatchOnly() {
	// Operator has FEEDBACK_FORM_FIELDS configured for headline + summary, but no live
	// interpretation ever ran (no TAC follow-ups). The URL should still build, just
	// without the summary-derived fields.
	formURL := "https://docs.google.com/forms/d/e/abc/viewform"
	fields := `{"tac_channel":"entry.111","headline":"entry.444","dispatch_transcript":"entry.333"}`
	tc := s.newClientForFeedbackTests(formURL, fields)

	got := tc.buildFeedbackURL(s.ctx, "1967", ClosureMeta{
		TACChannel:    "TAC10",
		Transcription: "Original dispatch text.",
	}, time.Now())

	parsed, err := url.Parse(got)
	s.Require().NoError(err)
	q := parsed.Query()
	s.Equal("TAC10", q.Get("entry.111"))
	s.Equal("Original dispatch text.", q.Get("entry.333"))
	s.Empty(q.Get("entry.444"), "no summary_data → headline must not be set (not even an empty entry)")
}

func (s *DispatchSuite) TestBuildFeedbackURL_BaseURLAlreadyHasQuery_AppendsWithAmpersand() {
	formURL := "https://docs.google.com/forms/d/e/abc/viewform?usp=pp_url"
	fields := `{"tac_channel":"entry.111"}`
	tc := s.newClientForFeedbackTests(formURL, fields)

	got := tc.buildFeedbackURL(s.ctx, "1967", ClosureMeta{TACChannel: "TAC10"}, time.Now())
	s.Contains(got, "?usp=pp_url&entry.111=TAC10", "must use & when base already has ?")
	s.NotContains(got, "??", "must not double-?")
}

func (s *DispatchSuite) TestBuildFeedbackURL_MalformedFieldsJSON_FallsBackToBareURL() {
	formURL := "https://docs.google.com/forms/d/e/abc/viewform"
	tc := s.newClientForFeedbackTests(formURL, `{not actually json`)
	got := tc.buildFeedbackURL(s.ctx, "1967", ClosureMeta{TACChannel: "TAC10"}, time.Now())
	s.Equal(formURL, got, "bad JSON → graceful fallback to bare URL, button still appears")
}

// newClientForFeedbackTests is a focused constructor that wires only the bits
// buildFeedbackURL touches — Dragonfly + Config — without standing up Slack/ML mocks.
func (s *DispatchSuite) newClientForFeedbackTests(formURL, formFields string) *TranscribeClient {
	dfly, err := dragonfly.NewClient(s.ctx, 2*time.Second, &redis.Options{Addr: s.redisAddr})
	require.NoError(s.T(), err)
	return &TranscribeClient{
		dragonflyClient: dfly,
		config: &config.Config{
			FeedbackFormURL:    formURL,
			FeedbackFormFields: formFields,
		},
	}
}
