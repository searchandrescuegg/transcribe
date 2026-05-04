package ml

import "context"

type DispatchMessages struct {
	Messages      []DispatchMessage `json:"messages"`
	Transcription string            `json:"transcription"`
}
type DispatchMessage struct {
	CallType             string `json:"call_type"`
	TACChannel           string `json:"tac_channel"`
	CleanedTranscription string `json:"cleaned_transcription"`
}

// FIX (review item #3): added ctx parameter so caller-imposed deadlines (WorkerTimeout) and
// shutdown cancellation propagate into LLM calls; previously implementations used context.Background().
type DispatchMessageParser interface {
	ParseRelevantInformationFromDispatchMessage(ctx context.Context, transcription string) (*DispatchMessages, error)
}

// TACTranscript is one TAC channel transmission with its capture time. Capture time comes
// from the SDR-Trunk filename (the audio's recorded-at moment, not when it was processed),
// so it's stable regardless of pipeline latency.
type TACTranscript struct {
	CapturedAt string `json:"captured_at"` // ISO-8601 or HH:MM:SS — the LLM treats it as opaque text
	Text       string `json:"text"`
}

// RescueSummaryInput bundles every piece of context the summarizer needs. The dispatch
// transcript anchors what kind of incident this is; the TAC transcripts (in chronological
// order) carry the operational chatter.
type RescueSummaryInput struct {
	DispatchTranscription string
	DispatchCallType      string // "Rescue - Trail", etc. — what the dispatch parser already extracted
	TACChannel            string // "TAC10", etc.
	TACTranscripts        []TACTranscript
}

// RescueSummary is the structured wrap-up for a rescue. Each field is independently
// renderable as a Slack section; KeyEvents lets us produce a timeline.
type RescueSummary struct {
	// Headline is one short sentence suitable for a Slack message title or notification
	// preview. Should read as "what happened" at a glance.
	Headline string `json:"headline"`

	// SituationSummary is a 1–3 sentence narrative summary of the incident.
	SituationSummary string `json:"situation_summary"`

	// Location is the best-known location of the incident as referenced in the chatter.
	// May include landmark names ("Tiger Mountain Trail near the OTG turnoff"), addresses,
	// or coordinates if mentioned. Empty if unclear.
	Location string `json:"location"`

	// UnitsInvolved is the list of responding units mentioned by name (e.g. "Engine 8171",
	// "Battalion 171", "Medic 104"). Best-effort deduplication.
	UnitsInvolved []string `json:"units_involved"`

	// PatientStatus is a short description of patient condition / outcome if mentioned
	// ("ambulatory", "transported to Bellevue", "no patient contact"). Empty if not stated.
	PatientStatus string `json:"patient_status"`

	// Outcome is the disposition of the call: "Resolved — patient transported", "Cancelled
	// en route", "False alarm", "Ongoing", etc. The model picks from observed cues.
	Outcome string `json:"outcome"`

	// KeyEvents is a chronological list of notable moments. CapturedAt mirrors the input
	// transcript timestamp; Description is a short sentence.
	KeyEvents []RescueSummaryEvent `json:"key_events"`
}

type RescueSummaryEvent struct {
	CapturedAt  string `json:"captured_at"`
	Description string `json:"description"`
}

// RescueSummarizer turns a dispatch + TAC transcripts into a structured RescueSummary.
// Same context-propagation discipline as DispatchMessageParser.
type RescueSummarizer interface {
	SummarizeRescue(ctx context.Context, input RescueSummaryInput) (*RescueSummary, error)
}
