package prompts

import (
	"fmt"

	"github.com/sashabaranov/go-openai/jsonschema"
	"github.com/searchandrescuegg/transcribe/internal/ml"
)

// DispatchSchema generates the response schema for the dispatch parser and, when
// allowedCallTypes is non-empty, post-processes it to inject an `enum` constraint on the
// messages[].call_type field. Both the OpenAI-compatible backend (strict json_schema) and
// the Anthropic backend (output_config.format) consume this schema, so the enum becomes a
// hard contract on either path — the model can only emit a listed value or "Unknown".
//
// go-openai's GenerateSchemaForType already emits `additionalProperties: false` and a
// `required` array for every object, which is exactly what both providers' strict modes
// require, so the generated schema is usable verbatim.
func DispatchSchema(allowedCallTypes []string) (*jsonschema.Definition, error) {
	schema, err := jsonschema.GenerateSchemaForType(&ml.DispatchMessages{})
	if err != nil {
		return nil, err
	}
	if len(allowedCallTypes) == 0 {
		return schema, nil
	}

	enum := make([]string, 0, len(allowedCallTypes)+1)
	enum = append(enum, allowedCallTypes...)
	enum = append(enum, UnknownCallType)

	// Walk to messages[].call_type. As of go-openai v1.41+, GenerateSchemaForType emits a
	// `$ref` into a `$defs` table for nested struct types instead of inlining Properties on
	// Items. Resolve the ref so the enum patches the right node — and keep the legacy inline
	// path so older library versions still work.
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

// RescueSummarySchema generates the response schema for the rescue summarizer from the
// ml.RescueSummary struct. Renaming a field there propagates automatically; adding a field
// still requires updating the block builders and feedback URL code that read it (see
// CLAUDE.md).
func RescueSummarySchema() (*jsonschema.Definition, error) {
	return jsonschema.GenerateSchemaForType(&ml.RescueSummary{})
}

// TACCleanupSchema generates the response schema for the per-transmission TAC cleanup call
// from the ml.TACCleanupResult struct ({cleaned_text}). Same strict-mode-friendly shape as the
// other schemas.
func TACCleanupSchema() (*jsonschema.Definition, error) {
	return jsonschema.GenerateSchemaForType(&ml.TACCleanupResult{})
}
