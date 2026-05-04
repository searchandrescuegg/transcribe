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

// unknownCallType is appended to any caller-supplied allowed-call-types list so the model
// always has an escape hatch when it can't classify, instead of being forced to fabricate.
const unknownCallType = "Unknown"

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
			{Role: openai.ChatMessageRoleSystem, Content: oc.systemPrompt()},
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

// systemPrompt returns the system prompt sent with each request. When allowedCallTypes is
// non-empty the prompt is rewritten to reference the canonical list (the list itself is
// inlined so the model knows the spelling and casing it MUST emit). When empty, the prompt
// keeps the in-line examples for backward compatibility.
func (oc *OpenAIClient) systemPrompt() string {
	if len(oc.allowedCallTypes) == 0 {
		return defaultSystemPrompt
	}
	// Use a clearly-delimited list so the model can't misinterpret comma-separated names
	// that themselves contain commas (e.g. "Rescue - Trail, Mountain"). One per line.
	var b strings.Builder
	b.WriteString(constrainedSystemPromptHead)
	b.WriteString("\nThe call_type field MUST be exactly one of the following (case-sensitive):\n")
	for _, ct := range oc.allowedCallTypes {
		b.WriteString("- ")
		b.WriteString(ct)
		b.WriteString("\n")
	}
	b.WriteString("- ")
	b.WriteString(unknownCallType)
	b.WriteString("\n")
	b.WriteString(constrainedSystemPromptTail)
	return b.String()
}

const defaultSystemPrompt = `You are a tool to accurately parse relevant information from a transcription of Fire Department radio messages.
You will need to extract the call type and the tactical channel (TAC) from the transcription, including the FULL transcription.
Please return the information in the defined format. There may be multiple calls within a single transcription, so if there are multiple calls, please identify and separate into multiple messages, but ensure they are deduplicated.
Call types can include "Aid Emergency", "MVC", "MVC Aid Emergency", "AFA Commercial", "Rescue - Trail", etc.
If the call type can not be determined, return "Unknown".
The tactical channel (TAC) should be in the format "TAC1", "TAC2", etc. Do not include a space between "TAC" and the number. If it appears as SPFR Repeater, assume it is "TAC8".
Please clean the transcription to update any misspellings, incorrect locations, and generally ensure that it is clear and concise.
Do not add any additional information or context that is not present in the transcription.`

const constrainedSystemPromptHead = `You are a tool to accurately parse relevant information from a transcription of Fire Department radio messages.
You will need to extract the call type and the tactical channel (TAC) from the transcription, including the FULL transcription.
Please return the information in the defined format. There may be multiple calls within a single transcription, so if there are multiple calls, please identify and separate into multiple messages, but ensure they are deduplicated.`

const constrainedSystemPromptTail = `If the call type cannot be confidently mapped to one of the values listed above, return "Unknown".
The tactical channel (TAC) should be in the format "TAC1", "TAC2", etc. Do not include a space between "TAC" and the number. If it appears as SPFR Repeater, assume it is "TAC8".
Please clean the transcription to update any misspellings, incorrect locations, and generally ensure that it is clear and concise.
Do not add any additional information or context that is not present in the transcription.`

// buildSchema generates the response schema and, when allowedCallTypes is non-empty, post-
// processes it to inject an `enum` constraint on the messages[].call_type field. With
// `Strict: true` on the OpenAI side, this becomes a hard contract: the model can only emit
// values from the enum, eliminating the prior need for fuzzy matching downstream.
func (oc *OpenAIClient) buildSchema() (*jsonschema.Definition, error) {
	schema, err := jsonschema.GenerateSchemaForType(&ml.DispatchMessages{})
	if err != nil {
		return nil, err
	}
	if len(oc.allowedCallTypes) == 0 {
		return schema, nil
	}

	enum := make([]string, 0, len(oc.allowedCallTypes)+1)
	enum = append(enum, oc.allowedCallTypes...)
	enum = append(enum, unknownCallType)

	// Walk to messages[].call_type. As of go-openai v1.41+, GenerateSchemaForType
	// emits a `$ref` into a `$defs` table for nested struct types instead of inlining
	// Properties on Items. Resolve the ref so the enum patches the right node — and
	// keep the legacy inline path so older library versions still work.
	messagesProp, ok := schema.Properties["messages"]
	if !ok {
		return nil, fmt.Errorf("schema missing expected `messages` property; library shape changed?")
	}
	if messagesProp.Items == nil {
		return nil, fmt.Errorf("schema `messages` has no Items definition; library shape changed?")
	}

	const dispatchMessageRef = "#/$defs/DispatchMessage"
	if messagesProp.Items.Ref == dispatchMessageRef {
		def, ok := schema.Defs["DispatchMessage"]
		if !ok {
			return nil, fmt.Errorf("schema `$defs.DispatchMessage` not found; library shape changed?")
		}
		callType, ok := def.Properties["call_type"]
		if !ok {
			return nil, fmt.Errorf("schema `$defs.DispatchMessage.call_type` not found; library shape changed?")
		}
		callType.Enum = enum
		def.Properties["call_type"] = callType
		schema.Defs["DispatchMessage"] = def
		return schema, nil
	}

	callType, ok := messagesProp.Items.Properties["call_type"]
	if !ok {
		return nil, fmt.Errorf("schema `messages.call_type` not found; library shape changed?")
	}
	callType.Enum = enum
	messagesProp.Items.Properties["call_type"] = callType
	schema.Properties["messages"] = messagesProp
	return schema, nil
}

// SummarizeRescue turns a dispatch + ordered TAC transcripts into a structured RescueSummary.
// Uses the same OpenAI client + structured-output discipline as the dispatch parser; the
// chat_template_kwargs.enable_thinking flag and post-hoc <think>-stripping are inherited
// because they apply to the OpenAI client, not per-method.
//
// Iterating on the prompt: the heart of this method is `rescueSummarySystemPrompt` below.
// Edit that string to tune the model's behavior, then re-run the iteration CLI
// (cmd/test-summary). The structured output schema is generated from the
// ml.RescueSummary struct, so renaming fields there propagates automatically.
func (oc *OpenAIClient) SummarizeRescue(ctx context.Context, input ml.RescueSummaryInput) (*ml.RescueSummary, error) {
	if input.DispatchTranscription == "" && len(input.TACTranscripts) == 0 {
		return nil, fmt.Errorf("no transcripts to summarize")
	}

	schema, err := jsonschema.GenerateSchemaForType(&ml.RescueSummary{})
	if err != nil {
		return nil, fmt.Errorf("failed to generate summary schema: %w", err)
	}

	userPrompt := buildRescueSummaryUserPrompt(input)

	req := openai.ChatCompletionRequest{
		Model: oc.model,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: rescueSummarySystemPrompt},
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

// rescueSummarySystemPrompt is the canonical instruction the model gets every time. Iterate
// on this string to tune the summary's voice, completeness, and accuracy. Pair edits with
// runs of cmd/test-summary against fixture inputs to see the effect.
const rescueSummarySystemPrompt = `You are an emergency-response analyst summarizing radio chatter from a US fire department's tactical channel during an in-progress rescue.

You will receive:
  - The original dispatch transcription that initiated the rescue (anchors what kind of incident this is).
  - The dispatched call type (e.g. "Rescue - Trail") and the assigned tactical channel (e.g. "TAC10").
  - An ordered list of TAC channel transmissions (one per radio key-up), each with a capture timestamp.

Produce a structured summary that lets a human responder catch up at a glance. Follow these rules strictly:

1. Be concise and factual. Do not speculate about details not stated in the transcripts.
2. The transcripts come from imperfect speech-to-text. Normalize obvious mistakes: "Italian one seventy one" → "Battalion 171"; "Mabel Valley" → "Maple Valley"; numeric units like "8171" likely mean Battalion 8171 or Engine 8171 — preserve as written if context is ambiguous.
3. Headline: one short sentence (≤ 80 chars) capturing the situation as it currently stands. Aim for what the on-call would want to know first.
4. SituationSummary: 1–3 sentences. What's the incident, where, who's responding, what's happening operationally.
5. Location: name the best-known location from the chatter (trailhead, address, mile marker). Empty string if not stated.
6. UnitsInvolved: list every responding unit mentioned (e.g. "Engine 8171", "Aid 151", "Battalion 171", "Medic 104"). Deduplicate. Use the canonical form, not the spoken form.
7. PatientStatus: one short phrase about patient condition or transport ("ambulatory; refused transport", "transported to Overlake", "no patient contact"). Empty if unstated.
8. Outcome: short phrase. Choose the most accurate from the chatter: "Ongoing", "Resolved — patient transported", "Cancelled en route", "False alarm", "Resources canceled by IC", etc. If transcripts mention units being canceled or "code 4 / wave-firm canceled", treat that as a resolution.
9. KeyEvents: an ordered list of notable moments with their CapturedAt timestamp from the transmission. Examples: "13:24:00 — Engine 8171 arrived on scene", "13:25:30 — Patient reached, ambulatory", "13:27:15 — Maple Valley resources canceled". Aim for 3–8 events; skip small acknowledgements ("copy", "171 received").
10. If the transcripts are too sparse to populate a field, return an empty string (or empty list for arrays) rather than fabricating.`

// buildRescueSummaryUserPrompt formats the input as a clearly-delimited block. The model
// performs better when the dispatch and TAC sections are explicitly labeled.
func buildRescueSummaryUserPrompt(input ml.RescueSummaryInput) string {
	var b strings.Builder
	b.WriteString("=== DISPATCH ===\n")
	if input.DispatchCallType != "" || input.TACChannel != "" {
		b.WriteString(fmt.Sprintf("Call type: %s\n", emptyAsDash(input.DispatchCallType)))
		b.WriteString(fmt.Sprintf("TAC channel: %s\n", emptyAsDash(input.TACChannel)))
		b.WriteString("\n")
	}
	b.WriteString("Transcript:\n")
	b.WriteString(input.DispatchTranscription)
	b.WriteString("\n\n=== TAC TRANSMISSIONS (chronological) ===\n")
	if len(input.TACTranscripts) == 0 {
		b.WriteString("(none yet)\n")
	}
	for i, t := range input.TACTranscripts {
		b.WriteString(fmt.Sprintf("[%d] %s — %s\n", i+1, emptyAsDash(t.CapturedAt), t.Text))
	}
	return b.String()
}

func emptyAsDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
