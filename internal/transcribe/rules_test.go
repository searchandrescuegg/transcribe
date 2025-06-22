package transcribe

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCallIsTrailRescue(t *testing.T) {
	tests := []struct {
		name     string
		calltype string
		want     bool
	}{
		{
			name:     "trail rescue lowercase",
			calltype: "trail rescue",
			want:     true,
		},
		{
			name:     "trail rescue uppercase",
			calltype: "TRAIL RESCUE",
			want:     true,
		},
		{
			name:     "trail rescue mixed case",
			calltype: "Trail Rescue",
			want:     true,
		},
		{
			name:     "rescue trail reversed order",
			calltype: "rescue trail",
			want:     true,
		},
		{
			name:     "trail rescue with extra words",
			calltype: "emergency trail rescue operation",
			want:     true,
		},
		{
			name:     "trail only",
			calltype: "trail",
			want:     false,
		},
		{
			name:     "rescue only",
			calltype: "rescue",
			want:     false,
		},
		{
			name:     "empty string",
			calltype: "",
			want:     false,
		},
		{
			name:     "unrelated call type",
			calltype: "Aid Emergency",
			want:     false,
		},
		{
			name:     "hyphen",
			calltype: "Rescue - Trail",
			want:     true,
		},
		{
			name:     "reverse hyphen",
			calltype: "Trail - Rescue",
			want:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CallIsTrailRescue(tt.calltype)
			assert.Equal(t, tt.want, got, "CallIsTrailRescue(%q)", tt.calltype)
		})
	}
}
