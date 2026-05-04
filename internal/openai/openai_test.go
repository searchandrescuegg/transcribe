package openai

import (
	"testing"

	"github.com/stretchr/testify/assert"
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
