package transcribe

import (
	"bytes"
	"context"
	"log/slog"
	"path"
	"time"

	"github.com/slack-go/slack"
)

const (
	// audioUploadTimeout bounds one upload (URL fetch + PUT + complete) so a slow Slack upload
	// can't eat the whole worker budget. It counts toward the worker's serial LLM-call-plus-upload
	// budget alongside TAC cleanup and the summary call — see CLAUDE.md invariant #7.
	audioUploadTimeout = 20 * time.Second
	// maxAudioAttachmentBytes skips pathological files; trunk-recorder calls are far smaller.
	maxAudioAttachmentBytes = 20 << 20
)

// audioTitle labels an audio reply so it reads as belonging to the post above it.
func audioTitle(channelName string, capturedAt time.Time) string {
	if capturedAt.IsZero() {
		return channelName
	}
	return channelName + " · " + capturedAt.Local().Format("15:04:05")
}

// attachAudio uploads a transcription's audio as a file reply in threadTS so responders can
// listen to what the ASR heard. Best-effort by design: the transcript post already succeeded,
// so any failure logs and returns — it must never fail or nack the record.
func (tc *TranscribeClient) attachAudio(ctx context.Context, threadTS, s3Key, title string, audio []byte) {
	if !tc.config.AudioAttachmentsEnabled || threadTS == "" || len(audio) == 0 {
		return
	}
	if len(audio) > maxAudioAttachmentBytes {
		slog.Info("audio attachment skipped: file too large",
			slog.String("key", s3Key), slog.Int("bytes", len(audio)))
		return
	}
	filename := path.Base(s3Key)
	if s3Key == "" || filename == "." || filename == "/" {
		filename = "audio.wav"
	}
	uploadCtx, cancel := context.WithTimeout(ctx, audioUploadTimeout)
	defer cancel()
	if _, err := tc.slackClient.UploadFileV2Context(uploadCtx, slack.UploadFileV2Parameters{
		Channel:         tc.config.SlackChannelID,
		ThreadTimestamp: threadTS,
		Reader:          bytes.NewReader(audio),
		FileSize:        len(audio),
		Filename:        filename,
		Title:           title,
	}); err != nil {
		slog.Warn("audio attachment failed",
			slog.String("error", err.Error()), slog.String("key", s3Key), slog.String("thread", threadTS))
	}
}
