package transcribe

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type DeconstructedKey struct {
	Talkgroup    string
	Time         time.Time
	FrequencyMHz float64
	Suffix       string
	FileType     string
}

// example key: "1183-1750542445_854412500.1-call_1871.wav"
// components: "<talkgroup>-<timestamp>_<frequency>.<suffix>.<filetype>"
//
// FIX (prefixed-key regression): trunk-recorder now uploads objects under a
// "YYYY/MM/DD/HH/<talkgroup>/" prefix (homelab c5e064d) instead of the bucket root.
// The parser has always operated on the bare filename — with a prefix present,
// "<talkgroup>-..." was preceded by the path, so tg[0] became the whole prefix
// (e.g. "2026/07/05/19/1967/1967") rather than the talkgroup. That parsed without
// error, so every object silently missed the allow-list (and never matched the
// FireDispatch1TGID), causing all traffic to be ack-and-dropped. Reduce to the
// basename first so both the flat and prefixed layouts parse identically.
func parseKey(key string) (*DeconstructedKey, error) {
	if idx := strings.LastIndexByte(key, '/'); idx != -1 {
		key = key[idx+1:]
	}

	initialParts := strings.Split(key, ".")
	if len(initialParts) != 3 {
		return nil, fmt.Errorf("invalid key format: %s", key)
	}

	fileType := initialParts[2] // Get the file type from the last part
	suffix := initialParts[1]

	tg := strings.Split(initialParts[0], "-")
	// FIX (review item #4): bounds-check before indexing tg[1]; a malformed key with no dash
	// (e.g. "1399.foo.wav") previously panicked the worker.
	if len(tg) < 2 {
		return nil, fmt.Errorf("invalid talkgroup-timestamp format: %s", initialParts[0])
	}
	talkgroup := tg[0] // The talkgroup is the first part before the dash

	tsfreq := tg[1] // The second part contains the timestamp and frequency
	timeAndFreq := strings.Split(tsfreq, "_")
	if len(timeAndFreq) != 2 {
		return nil, fmt.Errorf("invalid time and frequency format: %s", tg[1])
	}

	timeSecs, err := strconv.ParseInt(timeAndFreq[0], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("failed to parse timestamp: %w", err)
	}

	timestamp := time.Unix(timeSecs, 0)

	frequencyMHz, err := strconv.ParseFloat(timeAndFreq[1], 64)
	if err != nil {
		return nil, fmt.Errorf("failed to parse frequency: %w", err)
	}

	return &DeconstructedKey{
		Talkgroup:    talkgroup,
		Time:         timestamp,
		FrequencyMHz: frequencyMHz,
		Suffix:       suffix,
		FileType:     fileType,
	}, nil
}
