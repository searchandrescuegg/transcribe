package transcribe

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

// Entry kinds in tac_transcripts:<TGID>. Entries written before human corrections existed have
// no kind and are radio transmissions.
const (
	entryKindRadio    = "radio"
	entryKindOperator = "operator"
	// entryKindInvalid marks an element that failed to decode. readEntries keeps it in place so
	// slice indexes stay aligned with Dragonfly list indexes for LSET.
	entryKindInvalid = "invalid"
)

// TranscriptCorrection records a human edit of a transcription: who, when, and the text it
// replaced. Stored on radio entries and on ClosureMeta (dispatch), and rendered as the
// "Corrected by" label on the Slack post.
type TranscriptCorrection struct {
	By       string    `json:"by"`
	At       time.Time `json:"at"`
	Original string    `json:"original"`
}

// liveTranscriptEntry is one element of tac_transcripts:<TGID>: either a radio transmission
// (kind radio) or a human `correction:` thread message (kind operator). All fields beyond
// captured_at/text are optional so pre-feature entries decode unchanged. Raw ASR is NOT
// stored here — it is joinable in the dataset via s3_key.
type liveTranscriptEntry struct {
	CapturedAt string `json:"captured_at"`
	Text       string `json:"text"` // radio: cleaned text; operator: correction text, prefix stripped
	Kind       string `json:"kind,omitempty"`

	// Radio only.
	PostedAt   string                `json:"posted_at,omitempty"` // RFC3339 time shown on the Slack post, for re-render
	SlackTS    string                `json:"slack_ts,omitempty"`  // radio: thread post ts; operator: the correction message ts
	S3Key      string                `json:"s3_key,omitempty"`
	Corrected  string                `json:"corrected,omitempty"`
	Correction *TranscriptCorrection `json:"correction,omitempty"`

	// Operator only.
	Author  string `json:"author,omitempty"`
	Deleted bool   `json:"deleted,omitempty"`
}

func (e liveTranscriptEntry) kind() string {
	if e.Kind == "" {
		return entryKindRadio
	}
	return e.Kind
}

// effectiveText is what the summarizer should see for a radio entry: the human correction when
// one exists, else the cleaned transcription.
func (e liveTranscriptEntry) effectiveText() string {
	if e.Correction != nil {
		return e.Corrected
	}
	return e.Text
}

func transcriptsKey(tgid string) string { return fmt.Sprintf(tacTranscriptsKeyFmt, tgid) }

func (tc *TranscribeClient) transcriptsTTL() time.Duration {
	return 2 * tc.config.TacticalChannelActivationDuration
}

// readEntries decodes the whole list. Undecodable elements become entryKindInvalid
// placeholders so the returned slice index equals the Dragonfly list index.
func (tc *TranscribeClient) readEntries(ctx context.Context, tgid string) ([]liveTranscriptEntry, error) {
	raw, err := tc.dragonflyClient.LRange(ctx, transcriptsKey(tgid), 0, -1)
	if err != nil {
		return nil, fmt.Errorf("LRange: %w", err)
	}
	entries := make([]liveTranscriptEntry, len(raw))
	for i, r := range raw {
		if err := json.Unmarshal([]byte(r), &entries[i]); err != nil {
			slog.Warn("live interpretation: unparseable transcript entry; keeping invalid placeholder",
				slog.String("error", err.Error()), slog.String("tgid", tgid), slog.Int("index", i))
			entries[i] = liveTranscriptEntry{Kind: entryKindInvalid}
		}
	}
	return entries, nil
}

// findEntryBySlackTS locates the entry of the given kind posted as slackTS.
func (tc *TranscribeClient) findEntryBySlackTS(ctx context.Context, tgid, slackTS, kind string) (int64, liveTranscriptEntry, bool, error) {
	if slackTS == "" {
		return 0, liveTranscriptEntry{}, false, nil
	}
	entries, err := tc.readEntries(ctx, tgid)
	if err != nil {
		return 0, liveTranscriptEntry{}, false, err
	}
	for i, e := range entries {
		if e.kind() == kind && e.SlackTS == slackTS {
			return int64(i), e, true, nil
		}
	}
	return 0, liveTranscriptEntry{}, false, nil
}

// setEntry overwrites one element in place. Indexes are stable because the list is
// append-only (retractions are tombstones, never LREM).
func (tc *TranscribeClient) setEntry(ctx context.Context, tgid string, idx int64, e liveTranscriptEntry) error {
	encoded, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal entry: %w", err)
	}
	if err := tc.dragonflyClient.LSet(ctx, transcriptsKey(tgid), idx, string(encoded)); err != nil {
		return fmt.Errorf("LSet: %w", err)
	}
	return nil
}

// appendEntry RPushes one entry and re-stamps the list TTL so an active rescue's history never
// expires under it.
func (tc *TranscribeClient) appendEntry(ctx context.Context, tgid string, e liveTranscriptEntry) error {
	encoded, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal entry: %w", err)
	}
	key := transcriptsKey(tgid)
	if err := tc.dragonflyClient.RPush(ctx, key, string(encoded)); err != nil {
		return fmt.Errorf("RPush: %w", err)
	}
	if err := tc.dragonflyClient.Expire(ctx, key, tc.transcriptsTTL()); err != nil {
		return fmt.Errorf("expire: %w", err)
	}
	return nil
}
