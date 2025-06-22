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
func parseKey(key string) (*DeconstructedKey, error) {
	initialParts := strings.Split(key, ".")
	if len(initialParts) != 3 {
		return nil, fmt.Errorf("invalid key format: %s", key)
	}

	fileType := initialParts[2] // Get the file type from the last part
	suffix := initialParts[1]

	tg := strings.Split(initialParts[0], "-")
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
