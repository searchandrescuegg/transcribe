// Package anthropic implements the ML backend against the first-party Anthropic API using
// native structured outputs (output_config.format). It satisfies both
// ml.DispatchMessageParser and ml.RescueSummarizer — the same two-interface contract the
// OpenAI-compatible backend implements — so it drops into transcribe.MLClient unchanged.
//
// The dispatch parser and rescue summarizer can run on different models (a cheap classifier
// for the high-volume dispatch feed, a stronger model for the lower-volume summaries); both
// share the prompt + schema contract in internal/prompts so the two backends never drift.
package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/sashabaranov/go-openai/jsonschema"
	"github.com/searchandrescuegg/transcribe/internal/ml"
	"github.com/searchandrescuegg/transcribe/internal/prompts"
)

// Client is the Anthropic-backed implementation of transcribe.MLClient.
type Client struct {
	client        anthropic.Client
	dispatchModel anthropic.Model
	summaryModel  anthropic.Model
	cleanupModel  anthropic.Model

	// allowedCallTypes is the canonical dispatch call-type list (already decrypted). When
	// non-empty the dispatch system prompt references it and the response schema constrains
	// call_type to an enum of these values plus "Unknown"; empty means no enum constraint.
	allowedCallTypes []string

	// maxTokens caps the structured-output response. The dispatch and summary payloads are
	// small (a cleaned transcription, or a headline + a handful of key events), so a modest
	// ceiling is plenty of headroom while keeping the request non-streaming.
	maxTokens int64
}

// Options configures the Anthropic client.
type Options struct {
	APIKey           string
	BaseURL          string // optional; empty uses the SDK default (https://api.anthropic.com)
	DispatchModel    string // e.g. "claude-haiku-4-5"
	SummaryModel     string // e.g. "claude-sonnet-5"
	CleanupModel     string // e.g. "claude-haiku-4-5"; empty falls back to DispatchModel
	AllowedCallTypes []string
	// Timeout bounds each request. Keep it <= WorkerTimeout so the worker context doesn't
	// cancel an in-flight call before it can answer (see CLAUDE.md invariant #7).
	Timeout   time.Duration
	MaxTokens int64
}

// NewClient constructs the Anthropic ML client.
func NewClient(opts Options) *Client {
	clientOpts := []option.RequestOption{option.WithAPIKey(opts.APIKey)}
	if opts.BaseURL != "" {
		clientOpts = append(clientOpts, option.WithBaseURL(opts.BaseURL))
	}
	if opts.Timeout > 0 {
		clientOpts = append(clientOpts, option.WithRequestTimeout(opts.Timeout))
	}

	maxTokens := opts.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 2048
	}

	cleanupModel := opts.CleanupModel
	if cleanupModel == "" {
		cleanupModel = opts.DispatchModel
	}

	return &Client{
		client:           anthropic.NewClient(clientOpts...),
		dispatchModel:    anthropic.Model(opts.DispatchModel),
		summaryModel:     anthropic.Model(opts.SummaryModel),
		cleanupModel:     anthropic.Model(cleanupModel),
		allowedCallTypes: opts.AllowedCallTypes,
		maxTokens:        maxTokens,
	}
}

// ParseRelevantInformationFromDispatchMessage classifies a fire-dispatch transcription into
// zero or more structured DispatchMessages via native structured output.
func (c *Client) ParseRelevantInformationFromDispatchMessage(ctx context.Context, transcription string) (*ml.DispatchMessages, error) {
	if transcription == "" {
		return nil, fmt.Errorf("transcription cannot be empty")
	}

	def, err := prompts.DispatchSchema(c.allowedCallTypes)
	if err != nil {
		return nil, fmt.Errorf("failed to build JSON schema: %w", err)
	}
	schema, err := schemaToMap(def)
	if err != nil {
		return nil, fmt.Errorf("failed to convert schema: %w", err)
	}

	raw, err := c.complete(ctx, c.dispatchModel, prompts.DispatchSystemPrompt(c.allowedCallTypes), transcription, schema)
	if err != nil {
		return nil, err
	}

	var dispatchMessages ml.DispatchMessages
	if err := json.Unmarshal([]byte(raw), &dispatchMessages); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w, content: %s", err, raw)
	}
	return &dispatchMessages, nil
}

// SummarizeRescue turns a dispatch + ordered TAC transcripts into a structured RescueSummary
// via native structured output.
func (c *Client) SummarizeRescue(ctx context.Context, input ml.RescueSummaryInput) (*ml.RescueSummary, error) {
	if input.DispatchTranscription == "" && len(input.TACTranscripts) == 0 {
		return nil, fmt.Errorf("no transcripts to summarize")
	}

	def, err := prompts.RescueSummarySchema()
	if err != nil {
		return nil, fmt.Errorf("failed to generate summary schema: %w", err)
	}
	schema, err := schemaToMap(def)
	if err != nil {
		return nil, fmt.Errorf("failed to convert schema: %w", err)
	}

	raw, err := c.complete(ctx, c.summaryModel, prompts.RescueSummarySystemPrompt, prompts.BuildRescueSummaryUserPrompt(input), schema)
	if err != nil {
		return nil, fmt.Errorf("rescue summary: %w", err)
	}

	var summary ml.RescueSummary
	if err := json.Unmarshal([]byte(raw), &summary); err != nil {
		return nil, fmt.Errorf("failed to unmarshal rescue summary: %w, content: %s", err, raw)
	}
	return &summary, nil
}

// CleanTACTranscript rewrites one raw TAC transmission into a clean, faithful transcription via
// native structured output. Runs on the cleanup model (defaults to the cheap/fast dispatch tier,
// overridable via ANTHROPIC_CLEANUP_MODEL) — cleanup is high-volume, one call per transmission.
func (c *Client) CleanTACTranscript(ctx context.Context, in ml.TACCleanupInput) (*ml.TACCleanupResult, error) {
	if in.Text == "" {
		return nil, fmt.Errorf("transcription cannot be empty")
	}

	def, err := prompts.TACCleanupSchema()
	if err != nil {
		return nil, fmt.Errorf("failed to generate cleanup schema: %w", err)
	}
	schema, err := schemaToMap(def)
	if err != nil {
		return nil, fmt.Errorf("failed to convert schema: %w", err)
	}

	raw, err := c.complete(ctx, c.cleanupModel, prompts.TACCleanupSystemPrompt, prompts.BuildTACCleanupUserPrompt(in), schema)
	if err != nil {
		return nil, fmt.Errorf("tac cleanup: %w", err)
	}

	var result ml.TACCleanupResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal cleanup result: %w, content: %s", err, raw)
	}
	return &result, nil
}

// complete issues one non-streaming structured-output request and returns the raw JSON text.
//
// Thinking is disabled: these are short extraction/classification tasks where reasoning adds
// latency and cost without improving a schema-constrained answer, and it keeps the round-trip
// comfortably inside WorkerTimeout.
//
// The system prompt carries a cache_control breakpoint so the stable prefix is cached across
// calls. Note the cache only engages once the prefix exceeds the model's minimum cacheable
// size (4096 tokens for Haiku 4.5, 2048 for Sonnet 5); the current prompts sit below that, so
// caching is effectively a no-op today but kicks in automatically if a large CALL_TYPES enum
// or a longer prompt pushes the prefix past the threshold.
func (c *Client) complete(ctx context.Context, model anthropic.Model, systemPrompt, userContent string, schema map[string]any) (string, error) {
	resp, err := c.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     model,
		MaxTokens: c.maxTokens,
		System: []anthropic.TextBlockParam{{
			Text:         systemPrompt,
			CacheControl: anthropic.NewCacheControlEphemeralParam(),
		}},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(userContent)),
		},
		Thinking: anthropic.ThinkingConfigParamUnion{
			OfDisabled: &anthropic.ThinkingConfigDisabledParam{},
		},
		OutputConfig: anthropic.OutputConfigParam{
			Format: anthropic.JSONOutputFormatParam{Schema: schema},
		},
	})
	if err != nil {
		return "", fmt.Errorf("messages request error: %w", err)
	}

	text := extractText(resp)
	if text == "" {
		return "", fmt.Errorf("empty response content from Anthropic (stop_reason: %s)", resp.StopReason)
	}
	return text, nil
}

// extractText concatenates the text of every text content block in the response. Structured
// output returns a single JSON-bearing text block, but concatenating is robust to the model
// emitting more than one.
func extractText(resp *anthropic.Message) string {
	var b strings.Builder
	for _, block := range resp.Content {
		if t, ok := block.AsAny().(anthropic.TextBlock); ok {
			b.WriteString(t.Text)
		}
	}
	return strings.TrimSpace(b.String())
}

// schemaToMap converts a go-openai jsonschema.Definition into the map[string]any that the
// Anthropic SDK's output_config.format expects. Both backends generate the schema from the
// same struct via internal/prompts, so this is purely a representation change.
func schemaToMap(def *jsonschema.Definition) (map[string]any, error) {
	rawSchema, err := json.Marshal(def)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(rawSchema, &m); err != nil {
		return nil, err
	}
	return m, nil
}
