// Package dataset captures a corpus of ASR transcriptions and LLM interactions in Postgres
// for offline prompt refinement. It is entirely best-effort: capture is flag-gated
// (DATASET_ENABLED) and every write path drops rather than blocks, so nothing here can slow
// down or fail the radio-processing pipeline.
//
// Two capture surfaces:
//   - Raw transcriptions are recorded directly from the transcribe worker (processRecord).
//   - LLM interactions are recorded by RecordingMLClient, a transparent decorator that wraps
//     any transcribe.MLClient and logs each dispatch-parse / rescue-summary call's
//     input, structured output (or error), model, and system-prompt hash.
package dataset

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cespare/xxhash/v2"
	"github.com/searchandrescuegg/transcribe/internal/ml"
	"github.com/searchandrescuegg/transcribe/internal/prompts"
)

// TranscriptionRecord is one ASR transcription of an audio object.
type TranscriptionRecord struct {
	S3Key            string
	Talkgroup        string
	CapturedAt       time.Time
	Transcription    string
	NoSpeechDetected bool
	IsDispatch       bool
}

// LLMInteractionRecord is one dispatch-parse or rescue-summary model call.
type LLMInteractionRecord struct {
	Kind       string // "dispatch_parse" | "rescue_summary"
	Backend    string // "anthropic" | "openai"
	Model      string
	PromptHash string
	S3Key      string // from request context, may be empty
	Talkgroup  string // from request context, may be empty
	InputText  string
	Output     json.RawMessage // structured result; nil when the call errored
	Err        string          // non-empty when the call failed
	LatencyMS  int64
}

// Recorder is the sink the pipeline writes to. Implementations MUST be non-blocking and
// best-effort — a slow or unavailable backend must never stall the caller.
type Recorder interface {
	RecordTranscription(TranscriptionRecord)
	RecordLLMInteraction(LLMInteractionRecord)
	Close() error
}

// --- request-context correlation -------------------------------------------------------

type sourceKey struct{}

// Source ties an LLM interaction back to the audio object that triggered it.
type Source struct {
	S3Key     string
	Talkgroup string
}

// ContextWithSource stamps the originating audio object onto the context so the recording
// decorator downstream can correlate an LLM call with its source transcription.
func ContextWithSource(ctx context.Context, s3Key, talkgroup string) context.Context {
	return context.WithValue(ctx, sourceKey{}, Source{S3Key: s3Key, Talkgroup: talkgroup})
}

// SourceFromContext returns the stamped Source, or the zero value if none was set.
func SourceFromContext(ctx context.Context) Source {
	s, _ := ctx.Value(sourceKey{}).(Source)
	return s
}

// --- recording decorator ---------------------------------------------------------------

// mlClient is the capability contract the decorator wraps. It is structurally identical
// to transcribe.MLClient but declared here so this package doesn't import transcribe (which
// imports this one).
type mlClient interface {
	ml.DispatchMessageParser
	ml.RescueSummarizer
	ml.TranscriptCleaner
}

// DecoratorOptions carries the static metadata recorded alongside every LLM interaction.
type DecoratorOptions struct {
	Backend          string   // "anthropic" | "openai"
	DispatchModel    string   // model that runs the dispatch parser
	SummaryModel     string   // model that runs the rescue summarizer
	CleanupModel     string   // model that runs the per-transmission TAC cleanup
	AllowedCallTypes []string // needed to hash the exact dispatch system prompt in use
}

// RecordingMLClient wraps an MLClient and records every call. It implements both
// ml.DispatchMessageParser and ml.RescueSummarizer, so it satisfies transcribe.MLClient and
// drops in transparently. The wrapped call's result and error are always returned unchanged.
type RecordingMLClient struct {
	inner mlClient
	rec   Recorder
	opts  DecoratorOptions

	// Prompt hashes are stable for the life of the process, so compute them once.
	dispatchPromptHash string
	summaryPromptHash  string
	cleanupPromptHash  string
}

// NewRecordingMLClient wraps inner so each call is logged to rec.
func NewRecordingMLClient(inner mlClient, rec Recorder, opts DecoratorOptions) *RecordingMLClient {
	return &RecordingMLClient{
		inner:              inner,
		rec:                rec,
		opts:               opts,
		dispatchPromptHash: hashString(prompts.DispatchSystemPrompt(opts.AllowedCallTypes)),
		summaryPromptHash:  hashString(prompts.RescueSummarySystemPrompt),
		cleanupPromptHash:  hashString(prompts.TACCleanupSystemPrompt),
	}
}

func (r *RecordingMLClient) ParseRelevantInformationFromDispatchMessage(ctx context.Context, transcription string) (*ml.DispatchMessages, error) {
	start := time.Now()
	out, err := r.inner.ParseRelevantInformationFromDispatchMessage(ctx, transcription)
	r.record(ctx, "dispatch_parse", r.opts.DispatchModel, r.dispatchPromptHash, transcription, out, err, time.Since(start))
	return out, err
}

func (r *RecordingMLClient) SummarizeRescue(ctx context.Context, input ml.RescueSummaryInput) (*ml.RescueSummary, error) {
	start := time.Now()
	out, err := r.inner.SummarizeRescue(ctx, input)
	r.record(ctx, "rescue_summary", r.opts.SummaryModel, r.summaryPromptHash, prompts.BuildRescueSummaryUserPrompt(input), out, err, time.Since(start))
	return out, err
}

func (r *RecordingMLClient) CleanTACTranscript(ctx context.Context, in ml.TACCleanupInput) (*ml.TACCleanupResult, error) {
	start := time.Now()
	out, err := r.inner.CleanTACTranscript(ctx, in)
	r.record(ctx, "tac_cleanup", r.opts.CleanupModel, r.cleanupPromptHash, prompts.BuildTACCleanupUserPrompt(in), out, err, time.Since(start))
	return out, err
}

// record builds and enqueues an interaction record. Marshal failures degrade to a nil
// Output rather than dropping the whole record — the input text and error are still useful.
func (r *RecordingMLClient) record(ctx context.Context, kind, model, promptHash, input string, out any, callErr error, latency time.Duration) {
	if r.rec == nil {
		return
	}
	src := SourceFromContext(ctx)

	var output json.RawMessage
	if callErr == nil && out != nil {
		if raw, marshalErr := json.Marshal(out); marshalErr == nil {
			output = raw
		}
	}

	var errStr string
	if callErr != nil {
		errStr = callErr.Error()
	}

	r.rec.RecordLLMInteraction(LLMInteractionRecord{
		Kind:       kind,
		Backend:    r.opts.Backend,
		Model:      model,
		PromptHash: promptHash,
		S3Key:      src.S3Key,
		Talkgroup:  src.Talkgroup,
		InputText:  input,
		Output:     output,
		Err:        errStr,
		LatencyMS:  latency.Milliseconds(),
	})
}

func hashString(s string) string {
	return fmt.Sprintf("%016x", xxhash.Sum64String(s))
}
