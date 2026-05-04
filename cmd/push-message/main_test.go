package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseFilename(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
		tg   string
		ts   int64
	}{
		{"trunk-recorder format", "1399-1777832036_852162500.0-call_001.wav", true, "1399", 1777832036},
		{"longer suffix", "1967-1777832293_852162500.0-call_005.wav", true, "1967", 1777832293},
		{"missing underscore", "1399-1777832036.wav", false, "", 0},
		{"missing tg", "-1777832036_852162500.0-call_001.wav", false, "", 0},
		{"non-numeric tg", "abc-1777832036_852162500.0-call_001.wav", false, "", 0},
		{"empty", "", false, "", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := parseFilename(c.in)
			assert.Equal(t, c.ok, ok)
			if c.ok {
				assert.Equal(t, c.tg, got.talkgroup)
				assert.Equal(t, c.ts, got.timestamp)
			}
		})
	}
}

func TestCollectFixtures_SortsByTimestamp(t *testing.T) {
	dir := t.TempDir()
	// Drop them in reverse chronological order to prove the sort is real.
	for _, name := range []string{
		"1967-1777832293_852162500.0-call_005.wav",
		"1399-1777832036_852162500.0-call_001.wav",
		"1967-1777832063_852162500.0-call_002.wav",
		"some-other-file.txt",  // skipped: wrong extension
		"$version",             // skipped: no .wav suffix
		"unmatched-format.wav", // skipped: doesn't match the regex
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), nil, 0o600))
	}

	got, err := collectFixtures(dir, "")
	require.NoError(t, err)
	require.Len(t, got, 3, "only the three valid fixtures should be returned")
	assert.Equal(t, int64(1777832036), got[0].timestamp, "earliest first")
	assert.Equal(t, int64(1777832063), got[1].timestamp)
	assert.Equal(t, int64(1777832293), got[2].timestamp)
}

func TestCollectFixtures_OneShot(t *testing.T) {
	got, err := collectFixtures("/does/not/exist", "/some/path/1399-1777832036_852162500.0-call_001.wav")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "1399-1777832036_852162500.0-call_001.wav", got[0].key)
	assert.Equal(t, "1399", got[0].talkgroup)
}

func TestCollectFixtures_OneShotRejectsBadName(t *testing.T) {
	_, err := collectFixtures("", "demo.m4a")
	require.Error(t, err)
}
