package transcribe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/searchandrescuegg/transcribe/internal/dataset"
)

// ErrOperatorCorrectionUnchanged is returned by UpsertOperatorCorrection when an edit carries the
// exact text already stored for a live note (Slack re-sends message_changed for link unfurls and
// the like). Nothing was written and nothing was recorded; callers must treat it as "do nothing"
// — no summary refresh, no reaction.
var ErrOperatorCorrectionUnchanged = errors.New("operator correction unchanged")

// UpsertOperatorCorrection stores (or, for an already-seen slackTS, amends) an `@PSERN`
// thread note on the active rescue whose thread is threadTS. Returns created=true for a new
// note. The caller refreshes the summary with rewrite = !created: a new note is additive
// (rule 15 overrides conflicts), an edit may retract facts so it re-derives. An edit whose text
// matches the live note returns ErrOperatorCorrectionUnchanged (no write, no dataset row).
func (tc *TranscribeClient) UpsertOperatorCorrection(ctx context.Context, threadTS, slackTS, userID, text string, at time.Time) (string, bool, error) {
	tgid, _, ok, err := tc.LookupTGIDByThread(ctx, threadTS)
	if err != nil {
		return "", false, err
	}
	if !ok {
		return "", false, ErrRescueNotActive
	}

	idx, e, found, err := tc.findEntryBySlackTS(ctx, tgid, slackTS, entryKindOperator)
	if err != nil {
		return tgid, false, err
	}
	if found {
		if e.Text == text && !e.Deleted {
			return tgid, false, ErrOperatorCorrectionUnchanged
		}
		prior := e.Text
		e.Text, e.Deleted = text, false
		if err := tc.setEntry(ctx, tgid, idx, e); err != nil {
			return tgid, false, err
		}
		tc.recordHumanCorrection(dataset.HumanCorrectionRecord{Kind: dataset.HumanCorrectionKindOperator,
			Action: dataset.HumanCorrectionActionEdit, TGID: tgid, SlackTS: slackTS, SlackUserID: userID,
			PriorText: prior, CorrectedText: text})
		return tgid, false, nil
	}

	if err := tc.appendEntry(ctx, tgid, liveTranscriptEntry{
		CapturedAt: at.Local().Format("15:04:05"), Text: text, Kind: entryKindOperator, SlackTS: slackTS, Author: userID,
	}); err != nil {
		return tgid, false, err
	}
	tc.recordHumanCorrection(dataset.HumanCorrectionRecord{Kind: dataset.HumanCorrectionKindOperator,
		Action: dataset.HumanCorrectionActionCreate, TGID: tgid, SlackTS: slackTS, SlackUserID: userID, CorrectedText: text})
	slog.Info("corrections: operator note added", slog.String("tgid", tgid), slog.String("user", userID))
	return tgid, true, nil
}

// RemoveOperatorCorrection tombstones the note posted as slackTS (Slack message deleted, or
// edited so it no longer starts with a mention of the bot). removed=false when there was nothing live
// to remove. The caller refreshes with rewrite=true when removed.
func (tc *TranscribeClient) RemoveOperatorCorrection(ctx context.Context, threadTS, slackTS, userID string) (string, bool, error) {
	tgid, _, ok, err := tc.LookupTGIDByThread(ctx, threadTS)
	if err != nil || !ok {
		return "", false, err
	}
	idx, e, found, err := tc.findEntryBySlackTS(ctx, tgid, slackTS, entryKindOperator)
	if err != nil || !found || e.Deleted {
		return tgid, false, err
	}
	prior := e.Text
	e.Deleted = true
	if err := tc.setEntry(ctx, tgid, idx, e); err != nil {
		return tgid, false, err
	}
	tc.recordHumanCorrection(dataset.HumanCorrectionRecord{Kind: dataset.HumanCorrectionKindOperator,
		Action: dataset.HumanCorrectionActionDelete, TGID: tgid, SlackTS: slackTS, SlackUserID: userID, PriorText: prior})
	return tgid, true, nil
}

// MigrateOperatorCorrections copies live operator notes from oldTGID's list to newTGID's on a
// Switch TAC. Radio entries are intentionally NOT migrated (invariant #6 resets per-TAC radio
// context); human-verified notes describe the same incident and must survive.
func (tc *TranscribeClient) MigrateOperatorCorrections(ctx context.Context, oldTGID, newTGID string) error {
	entries, err := tc.readEntries(ctx, oldTGID)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.kind() != entryKindOperator || e.Deleted {
			continue
		}
		if err := tc.appendEntry(ctx, newTGID, e); err != nil {
			return fmt.Errorf("migrate note %s: %w", e.SlackTS, err)
		}
	}
	return nil
}
