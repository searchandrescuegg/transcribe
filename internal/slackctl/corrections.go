package slackctl

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/searchandrescuegg/transcribe/internal/transcribe"
)

// CorrectionService is the transcribe-side API the controller drives for human corrections.
// transcribe owns all state (entry schema, one-shot guard, summary refresh); the controller only
// adapts Slack payloads to these calls. *transcribe.TranscribeClient implements it.
// UpsertOperatorCorrection returns transcribe.ErrOperatorCorrectionUnchanged for an edit that
// doesn't change a live note's text; the controller treats that as "do nothing".
type CorrectionService interface {
	ResolveCorrectionTarget(ctx context.Context, messageTS, threadTS string) (transcribe.CorrectionTarget, error)
	ApplyTranscriptCorrection(ctx context.Context, target transcribe.CorrectionTarget, userID, newText string, now time.Time) error
	UpsertOperatorCorrection(ctx context.Context, threadTS, slackTS, userID, text string, at time.Time) (string, bool, error)
	RemoveOperatorCorrection(ctx context.Context, threadTS, slackTS, userID string) (string, bool, error)
	MigrateOperatorCorrections(ctx context.Context, oldTGID, newTGID string) error
	RefreshLiveInterpretation(ctx context.Context, tgid string, rewrite bool)
}

var _ CorrectionService = (*transcribe.TranscribeClient)(nil)

// correctionErrorMessage maps correction failures to user-facing text (ephemeral or modal error).
func correctionErrorMessage(err error) string {
	var already *transcribe.AlreadyCorrectedError
	switch {
	case errors.As(err, &already) && already.Correction.By == "":
		// The winner's correction couldn't be read back; don't render "<@> at 00:00".
		return ":information_source: This message has already been corrected — only one correction is allowed per message."
	case errors.As(err, &already):
		return fmt.Sprintf(":information_source: Already corrected by <@%s> at %s — only one correction is allowed per message.",
			already.Correction.By, already.Correction.At.Local().Format("15:04"))
	case errors.Is(err, transcribe.ErrRescueNotActive):
		return ":information_source: This rescue has closed; transcripts can no longer be corrected."
	case errors.Is(err, transcribe.ErrNotCorrectable):
		return ":information_source: Only dispatch and radio transcription posts can be corrected. (A transmission posted in the last few seconds may still be processing — try again shortly.)"
	case errors.Is(err, transcribe.ErrEmptyCorrection):
		return "The correction is empty."
	case errors.Is(err, transcribe.ErrUnchangedCorrection):
		return "The correction is unchanged from the current text."
	default:
		return ":warning: Correction failed; check service logs."
	}
}

// workCtx bounds async correction work (LLM summary included) like a pipeline worker.
func (c *Controller) workCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), c.cfg.WorkerTimeout)
}
