package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	openai "github.com/sashabaranov/go-openai"
	"github.com/sashabaranov/go-openai/jsonschema"
	"github.com/searchandrescuegg/transcribe/internal/ml"
	"github.com/searchandrescuegg/transcribe/internal/prompts"
)

// thinkBlockRE matches the chain-of-thought wrapper some reasoning-capable open-weights
// models emit before their final answer (Qwen3 with thinking enabled, certain Gemma
// fine-tunes, DeepSeek-R1, etc.). The structured-output schema asks for pure JSON, but
// when the served model decides to "think" it usually puts <think>...</think> ahead of
// the JSON and json.Unmarshal trips on the leading prose. (?s) makes . match newlines.
var thinkBlockRE = regexp.MustCompile(`(?s)<think>.*?</think>`)

// stripThinkingPrefix returns the JSON-bearing tail of a chat completion response. We
// strip <think>…</think> blocks unconditionally and then fall back to "first { onward"
// to catch the rare case where the model emits unfenced reasoning before the JSON
// (no <think> wrapper). If the response already starts with `{` (the common case for
// non-reasoning models like Gemma 4 E4B), both transforms are no-ops.
func stripThinkingPrefix(content string) string {
	content = thinkBlockRE.ReplaceAllString(content, "")
	content = strings.TrimSpace(content)
	if i := strings.Index(content, "{"); i > 0 {
		content = content[i:]
	}
	return content
}

// unknownCallType mirrors the shared fallback label so this package's tests can assert
// against it without importing the prompts package.
const unknownCallType = prompts.UnknownCallType

type OpenAIClient struct {
	client *openai.Client
	model  string

	// allowedCallTypes is the canonical list of dispatch call types loaded from the
	// confidential encrypted file. When non-empty the system prompt is rewritten to
	// reference this list and the response schema's `call_type` field is constrained
	// to an `enum` of these values plus "Unknown". Empty means the in-prompt examples
	// fall through and the schema imposes no enum constraint.
	allowedCallTypes []string

	// enableThinking controls whether the chat-template renders the model's chain-of-
	// thought (Qwen3-family templates). When false (default), every request carries
	// chat_template_kwargs: {"enable_thinking": false} — the canonical, server-side,
	// model-author-blessed switch. Templates that don't recognize the kwarg ignore it,
	// so this is safe to send to Gemma / Mistral / etc. without effect.
	enableThinking bool
}

// NewOpenAIClient constructs the OpenAI ML client. allowedCallTypes is the (already-
// decrypted) list of canonical call types; pass nil or an empty slice to skip both the
// prompt injection and the schema-level enum constraint. enableThinking=false (typical)
// suppresses Qwen3-style chain-of-thought emission server-side for faster responses.
func NewOpenAIClient(client *openai.Client, model string, allowedCallTypes []string, enableThinking bool) *OpenAIClient {
	return &OpenAIClient{
		client:           client,
		model:            model,
		allowedCallTypes: allowedCallTypes,
		enableThinking:   enableThinking,
	}
}

// FIX (review item #3): accepts caller's context so worker timeouts and shutdown cancel in-flight
// OpenAI calls; previously this used context.Background() which ignored WorkerTimeout entirely.
// FIX (review item #24): removed unreachable `if userContent == ""` branch since the empty-transcription
// guard above already returns; the dead defensive code was misleading.
func (oc *OpenAIClient) ParseRelevantInformationFromDispatchMessage(ctx context.Context, transcription string) (*ml.DispatchMessages, error) {
	if transcription == "" {
		return nil, fmt.Errorf("transcription cannot be empty")
	}

	dmSchema, err := oc.buildSchema()
	if err != nil {
		return nil, fmt.Errorf("failed to build JSON schema: %w", err)
	}

	req := openai.ChatCompletionRequest{
		Model: oc.model,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: prompts.DispatchSystemPrompt(oc.allowedCallTypes)},
			{Role: openai.ChatMessageRoleUser, Content: transcription},
		},
		ResponseFormat: &openai.ChatCompletionResponseFormat{
			Type: openai.ChatCompletionResponseFormatTypeJSONSchema,
			JSONSchema: &openai.ChatCompletionResponseFormatJSONSchema{
				Name:   "dispatch_message",
				Schema: dmSchema,
				Strict: true,
			},
		},
	}
	// First-party server-side switch for Qwen3-family models. The chat template reads
	// enable_thinking and skips <think>...</think> emission entirely when false. Templates
	// that don't recognize the kwarg (Gemma 4, Mistral, etc.) ignore it without error.
	if !oc.enableThinking {
		req.ChatTemplateKwargs = map[string]any{"enable_thinking": false}
	}

	resp, err := oc.client.CreateChatCompletion(ctx, req)

	if err != nil {
		return nil, fmt.Errorf("chat completion error: %w", err)
	}

	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("no response choices returned from OpenAI")
	}

	responseContent := resp.Choices[0].Message.Content
	if responseContent == "" {
		return nil, fmt.Errorf("empty response content from OpenAI")
	}
	cleaned := stripThinkingPrefix(responseContent)
	if cleaned == "" {
		return nil, fmt.Errorf("response content was nothing but reasoning prose: %s", responseContent)
	}

	var dispatchMessages ml.DispatchMessages
	if err := json.Unmarshal([]byte(cleaned), &dispatchMessages); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w, cleaned content: %s, raw content: %s", err, cleaned, responseContent)
	}

	return &dispatchMessages, nil
}

// buildSchema delegates to the shared prompts.DispatchSchema. It stays a method on
// OpenAIClient so the package's schema regression tests can exercise it directly.
func (oc *OpenAIClient) buildSchema() (*jsonschema.Definition, error) {
	return prompts.DispatchSchema(oc.allowedCallTypes)
}

// SummarizeRescue turns a dispatch + ordered TAC transcripts into a structured RescueSummary.
// Uses the same OpenAI client + structured-output discipline as the dispatch parser; the
// chat_template_kwargs.enable_thinking flag and post-hoc <think>-stripping are inherited
// because they apply to the OpenAI client, not per-method.
//
// Iterating on the prompt: edit prompts.RescueSummarySystemPrompt and re-run the iteration
// CLI (cmd/test-summary). The structured output schema is generated from the
// ml.RescueSummary struct, so renaming fields there propagates automatically.
func (oc *OpenAIClient) SummarizeRescue(ctx context.Context, input ml.RescueSummaryInput) (*ml.RescueSummary, error) {
	if input.DispatchTranscription == "" && len(input.TACTranscripts) == 0 {
		return nil, fmt.Errorf("no transcripts to summarize")
	}

	schema, err := prompts.RescueSummarySchema()
	if err != nil {
		return nil, fmt.Errorf("failed to generate summary schema: %w", err)
	}

	userPrompt := prompts.BuildRescueSummaryUserPrompt(input)

	req := openai.ChatCompletionRequest{
		Model: oc.model,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: prompts.RescueSummarySystemPrompt},
			{Role: openai.ChatMessageRoleUser, Content: userPrompt},
		},
		ResponseFormat: &openai.ChatCompletionResponseFormat{
			Type: openai.ChatCompletionResponseFormatTypeJSONSchema,
			JSONSchema: &openai.ChatCompletionResponseFormatJSONSchema{
				Name:   "rescue_summary",
				Schema: schema,
				Strict: true,
			},
		},
	}
	if !oc.enableThinking {
		req.ChatTemplateKwargs = map[string]any{"enable_thinking": false}
	}

	resp, err := oc.client.CreateChatCompletion(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("rescue summary chat completion: %w", err)
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("no response choices returned from OpenAI")
	}
	content := resp.Choices[0].Message.Content
	if content == "" {
		return nil, fmt.Errorf("empty response content from OpenAI")
	}
	cleaned := stripThinkingPrefix(content)
	if cleaned == "" {
		return nil, fmt.Errorf("response content was nothing but reasoning prose: %s", content)
	}

	var summary ml.RescueSummary
	if err := json.Unmarshal([]byte(cleaned), &summary); err != nil {
		return nil, fmt.Errorf("failed to unmarshal rescue summary: %w, cleaned content: %s, raw content: %s", err, cleaned, content)
	}
	return &summary, nil
}

// CleanTACTranscript rewrites one raw TAC transmission into a clean, faithful transcription using
// the same structured-output + <think>-stripping discipline as the other calls.
func (oc *OpenAIClient) CleanTACTranscript(ctx context.Context, in ml.TACCleanupInput) (*ml.TACCleanupResult, error) {
	if in.Text == "" {
		return nil, fmt.Errorf("transcription cannot be empty")
	}

	schema, err := prompts.TACCleanupSchema()
	if err != nil {
		return nil, fmt.Errorf("failed to generate cleanup schema: %w", err)
	}

	req := openai.ChatCompletionRequest{
		Model: oc.model,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: prompts.TACCleanupSystemPrompt},
			{Role: openai.ChatMessageRoleUser, Content: prompts.BuildTACCleanupUserPrompt(in)},
		},
		ResponseFormat: &openai.ChatCompletionResponseFormat{
			Type: openai.ChatCompletionResponseFormatTypeJSONSchema,
			JSONSchema: &openai.ChatCompletionResponseFormatJSONSchema{
				Name:   "tac_cleanup",
				Schema: schema,
				Strict: true,
			},
		},
	}
	if !oc.enableThinking {
		req.ChatTemplateKwargs = map[string]any{"enable_thinking": false}
	}

	resp, err := oc.client.CreateChatCompletion(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("tac cleanup chat completion: %w", err)
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("no response choices returned from OpenAI")
	}
	content := resp.Choices[0].Message.Content
	if content == "" {
		return nil, fmt.Errorf("empty response content from OpenAI")
	}
	cleaned := stripThinkingPrefix(content)
	if cleaned == "" {
		return nil, fmt.Errorf("response content was nothing but reasoning prose: %s", content)
	}

	var result ml.TACCleanupResult
	if err := json.Unmarshal([]byte(cleaned), &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal cleanup result: %w, cleaned content: %s, raw content: %s", err, cleaned, content)
	}
	return &result, nil
}
