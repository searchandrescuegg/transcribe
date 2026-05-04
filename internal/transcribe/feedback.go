package transcribe

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/searchandrescuegg/transcribe/internal/ml"
)

// Feedback-button URL building. The Google Form viewform URL accepts query parameters of
// the form `entry.NNNNNNNNN=value` to prefill specific questions. We map our logical
// closure fields (TAC channel, closed-at timestamp, dispatch transcript, headline,
// situation summary) to the operator-supplied entry IDs from FEEDBACK_FORM_FIELDS.
//
// Recognized logical names — adding one means populating it here AND letting operators
// know to map it in their JSON. Adding a new entry is intentionally a code change, not a
// runtime concern, so the set of available prefill fields is auditable in one place.
const (
	feedbackFieldTACChannel         = "tac_channel"
	feedbackFieldClosedAt           = "closed_at"
	feedbackFieldDispatchTranscript = "dispatch_transcript"
	feedbackFieldHeadline           = "headline"
	feedbackFieldSituationSummary   = "situation_summary"
)

// buildFeedbackURL returns the Google Form URL with prefilled query params, or "" if the
// feature isn't configured. Best-effort — bad config (malformed JSON, missing fields, etc.)
// degrades gracefully to the bare form URL or empty string rather than blowing up the
// closure path.
func (tc *TranscribeClient) buildFeedbackURL(ctx context.Context, tgid string, meta ClosureMeta, closedAt time.Time) string {
	if tc.config.FeedbackFormURL == "" {
		return ""
	}

	fieldMap := parseFeedbackFieldMap(tc.config.FeedbackFormFields)
	if len(fieldMap) == 0 {
		// Config has a URL but no field mapping — operator wants the button but no prefill.
		// Return the bare URL so the button still renders.
		return tc.config.FeedbackFormURL
	}

	// Pull the most recent structured summary (best-effort — empty if no TAC follow-ups
	// fired, or if Dragonfly hiccupped). Missing summary fields just don't get prefilled.
	var summary ml.RescueSummary
	if raw, err := tc.dragonflyClient.Get(ctx, fmt.Sprintf(summaryDataKeyFmt, tgid)); err == nil && raw != "" {
		if err := json.Unmarshal([]byte(raw), &summary); err != nil {
			slog.Warn("feedback URL: summary_data unparseable; falling back to dispatch-only prefill",
				slog.String("error", err.Error()), slog.String("tgid", tgid))
		}
	}

	values := url.Values{}
	addIfMapped := func(logical, value string) {
		if value == "" {
			return
		}
		if entry, ok := fieldMap[logical]; ok && entry != "" {
			values.Set(entry, value)
		}
	}

	addIfMapped(feedbackFieldTACChannel, meta.TACChannel)
	addIfMapped(feedbackFieldClosedAt, closedAt.Format("01/02/06 15:04 MST"))
	addIfMapped(feedbackFieldDispatchTranscript, meta.Transcription)
	addIfMapped(feedbackFieldHeadline, summary.Headline)
	addIfMapped(feedbackFieldSituationSummary, summary.SituationSummary)

	if len(values) == 0 {
		return tc.config.FeedbackFormURL
	}

	// The form URL might already contain query params (e.g. Google sometimes adds
	// `?usp=pp_url` automatically when copying a prefill link). Use the right separator.
	sep := "?"
	if strings.Contains(tc.config.FeedbackFormURL, "?") {
		sep = "&"
	}
	return tc.config.FeedbackFormURL + sep + values.Encode()
}

// parseFeedbackFieldMap unmarshals the JSON config into a logical-name → entry-id map.
// Returns nil on any error (caller treats nil as "no mapping configured"). Logged at warn
// so operators can see in the boot logs that their config is bad — we don't fail startup
// because the feedback feature is opt-in and shouldn't block a working pipeline.
func parseFeedbackFieldMap(raw string) map[string]string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		slog.Warn("FEEDBACK_FORM_FIELDS is not valid JSON; feedback button will render without prefill",
			slog.String("error", err.Error()))
		return nil
	}
	return m
}
