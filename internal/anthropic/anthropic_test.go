package anthropic

import (
	"testing"

	"github.com/searchandrescuegg/transcribe/internal/ml"
	"github.com/searchandrescuegg/transcribe/internal/prompts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Compile-time proof the Anthropic client satisfies both halves of transcribe.MLClient.
var (
	_ ml.DispatchMessageParser = (*Client)(nil)
	_ ml.RescueSummarizer      = (*Client)(nil)
)

// schemaToMap must preserve the strict-mode keys Anthropic's output_config.format requires:
// additionalProperties:false and a required array on every object.
func TestSchemaToMap_PreservesStrictModeKeys(t *testing.T) {
	def, err := prompts.RescueSummarySchema()
	require.NoError(t, err)

	m, err := schemaToMap(def)
	require.NoError(t, err)

	assert.Equal(t, "object", m["type"])
	assert.Equal(t, false, m["additionalProperties"], "strict structured output requires additionalProperties:false")
	assert.Contains(t, m, "properties")
	assert.Contains(t, m, "required")
}

// The call_type enum injected by prompts.DispatchSchema must survive the map round-trip,
// since that constraint is what makes the model's classification a hard contract.
func TestSchemaToMap_CarriesCallTypeEnum(t *testing.T) {
	allowed := []string{"Rescue - Trail", "Aid Emergency"}
	def, err := prompts.DispatchSchema(allowed)
	require.NoError(t, err)

	m, err := schemaToMap(def)
	require.NoError(t, err)

	defs, ok := m["$defs"].(map[string]any)
	require.True(t, ok, "expected $defs table in generated schema")
	dm, ok := defs["DispatchMessage"].(map[string]any)
	require.True(t, ok, "expected $defs.DispatchMessage")
	props, ok := dm["properties"].(map[string]any)
	require.True(t, ok)
	callType, ok := props["call_type"].(map[string]any)
	require.True(t, ok)

	enum, ok := callType["enum"].([]any)
	require.True(t, ok, "call_type must carry an enum after schema conversion")
	assert.Contains(t, enum, "Rescue - Trail")
	assert.Contains(t, enum, prompts.UnknownCallType)
}
