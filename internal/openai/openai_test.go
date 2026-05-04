package openai

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStripThinkingPrefix(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "pure JSON unchanged (Gemma 4 E4B common case)",
			in:   `{"messages":[{"call_type":"Rescue - Trail"}]}`,
			want: `{"messages":[{"call_type":"Rescue - Trail"}]}`,
		},
		{
			name: "single think block stripped",
			in:   "<think>Hmm, this looks like a trail rescue.</think>\n{\"messages\":[]}",
			want: `{"messages":[]}`,
		},
		{
			name: "multi-line think block stripped",
			in:   "<think>\nLet me reason about this.\nIt mentions TAC10 and rescue trail.\n</think>{\"messages\":[]}",
			want: `{"messages":[]}`,
		},
		{
			name: "multiple think blocks stripped",
			in:   "<think>first</think>between<think>second</think>{\"x\":1}",
			want: `{"x":1}`,
		},
		{
			name: "unfenced reasoning before JSON gets dropped via { fallback",
			in:   "Let me analyze:\nThis appears to be a trail rescue.\n\n{\"messages\":[]}",
			want: `{"messages":[]}`,
		},
		{
			name: "leading whitespace trimmed",
			in:   "   \n\t{\"x\":1}",
			want: `{"x":1}`,
		},
		{
			name: "no JSON at all",
			in:   "<think>just thinking, no answer</think>",
			want: ``,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, stripThinkingPrefix(c.in))
		})
	}
}

// Regression guard for the schema-walker bug shipped in v1.1.0: as of go-openai v1.41+,
// GenerateSchemaForType emits a `$ref` into a `$defs` table for nested struct types
// instead of inlining Properties on Items. The pre-fix walker only checked the inline
// path and crashed every dispatch parse with "schema `messages.call_type` not found".
func TestBuildSchema_AppliesEnumAtCallTypeRef(t *testing.T) {
	allowed := []string{"Rescue - Trail", "Aid Emergency", "Fire - Structure"}
	oc := &OpenAIClient{allowedCallTypes: allowed}

	schema, err := oc.buildSchema()
	require.NoError(t, err, "buildSchema must succeed with v1.41+ schema shape (ref + $defs)")
	require.NotNil(t, schema)

	// The library indirects through $defs; the enum must land on the resolved node.
	def, ok := schema.Defs["DispatchMessage"]
	require.True(t, ok, "expected $defs.DispatchMessage to exist on the generated schema")
	callType, ok := def.Properties["call_type"]
	require.True(t, ok, "expected $defs.DispatchMessage.call_type")

	want := append([]string{}, allowed...)
	want = append(want, unknownCallType)
	assert.Equal(t, want, callType.Enum, "enum must include allowed types plus the Unknown fallback")
}

func TestBuildSchema_NoAllowedCallTypes_ReturnsUnconstrainedSchema(t *testing.T) {
	oc := &OpenAIClient{} // empty allowedCallTypes
	schema, err := oc.buildSchema()
	require.NoError(t, err)
	require.NotNil(t, schema)

	if def, ok := schema.Defs["DispatchMessage"]; ok {
		callType := def.Properties["call_type"]
		assert.Empty(t, callType.Enum, "no allowed list configured → no enum constraint")
	}
}
