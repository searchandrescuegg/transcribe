package transcribe

import "testing"

// TestParseKeyTalkgroup guards the talkgroup extraction against upload-path drift.
// The parser must yield the same talkgroup whether the object sits at the bucket root
// (the original layout) or under a "YYYY/MM/DD/HH/<talkgroup>/" prefix (homelab c5e064d).
// Before the basename reduction, the prefixed form parsed without error but produced the
// whole path as the talkgroup, silently ack-and-dropping every object.
func TestParseKeyTalkgroup(t *testing.T) {
	tests := []struct {
		name          string
		key           string
		wantTalkgroup string
		wantErr       bool
	}{
		{
			name:          "flat key at bucket root",
			key:           "1399-1750542445_854412500.1-call_1871.wav",
			wantTalkgroup: "1399",
		},
		{
			name:          "date/talkgroup-prefixed key",
			key:           "2026/07/05/19/1967/1967-1783279634_853525000.1-call_66094.wav",
			wantTalkgroup: "1967",
		},
		{
			name:          "prefixed dispatch key still resolves to dispatch TGID",
			key:           "2026/07/05/10/1399/1399-1783279634_854412500.0-call_42.wav",
			wantTalkgroup: FireDispatch1TGID,
		},
		{
			name:    "no dash in filename",
			key:     "2026/07/05/1399.foo.wav",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseKey(tt.key)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseKey(%q) = %+v, want error", tt.key, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseKey(%q) unexpected error: %v", tt.key, err)
			}
			if got.Talkgroup != tt.wantTalkgroup {
				t.Errorf("parseKey(%q) talkgroup = %q, want %q", tt.key, got.Talkgroup, tt.wantTalkgroup)
			}
		})
	}
}
