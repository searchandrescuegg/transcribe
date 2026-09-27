package transcribe

import (
	"context"
	"log/slog"
	"time"

	"github.com/slack-go/slack"
)

// rerenderParentAlert rebuilds the live (not closed) rescue alert from meta — transcription,
// dispatch-correction label, SAR badge, status line, action buttons — and chat.updates it.
// Shared by the SAR-badge transition and dispatch corrections so neither drops the other's
// decoration. The current expiry comes from active_tacs; if it can't be read the rescue is
// closing, so skip. Best-effort: returns false on any failure.
func (tc *TranscribeClient) rerenderParentAlert(ctx context.Context, meta ClosureMeta, sarNotified bool, fallback string) bool {
	if meta.MessageTS == "" || meta.Transcription == "" {
		return false
	}
	score, err := tc.dragonflyClient.ZScore(ctx, activeTACsKey, meta.TGID)
	if err != nil {
		slog.Warn("parent alert re-render skipped; could not read expiry from active_tacs",
			slog.String("error", err.Error()), slog.String("tgid", meta.TGID))
		return false
	}
	blocks := BuildRescueTrailBlocks(&RescueTrailBlocksInput{
		TACChannel:        meta.TACChannel,
		TranscriptionText: meta.Transcription,
		ExpiresAt:         time.Unix(int64(score), 0).Local(),
		DispatchTGID:      FireDispatch1TGID,
		TACTalkgroupTGID:  meta.TGID,
		SARNotified:       sarNotified,
		Correction:        meta.DispatchCorrection,
	})
	updateCtx, cancel := context.WithTimeout(ctx, tc.config.SlackTimeout)
	defer cancel()
	if _, _, _, err := tc.slackClient.UpdateMessageContext(updateCtx, tc.config.SlackChannelID, meta.MessageTS,
		slack.MsgOptionBlocks(blocks...), slack.MsgOptionText(fallback, false)); err != nil {
		slog.Warn("parent alert re-render failed",
			slog.String("error", err.Error()), slog.String("tgid", meta.TGID), slog.String("message_ts", meta.MessageTS))
		return false
	}
	return true
}
