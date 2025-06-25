package transcribe

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/searchandrescuegg/transcribe/internal/config"
	"github.com/searchandrescuegg/transcribe/internal/dragonfly"
	"github.com/searchandrescuegg/transcribe/internal/ollama"
	"github.com/searchandrescuegg/transcribe/internal/pulsar"
	"github.com/searchandrescuegg/transcribe/internal/s3"
	"github.com/searchandrescuegg/transcribe/pkg/asr"
	"github.com/slack-go/slack"
	"github.com/versity/versitygw/s3event"
)

type TranscribeClient struct {
	pulsarClient    *pulsar.PulsarClient
	s3Client        *s3.S3Client
	asrClient       *asr.ASRClient
	ollamaClient    *ollama.OllamaClient
	slackClient     *slack.Client
	dragonflyClient *dragonfly.DragonflyClient

	config *config.Config
}

func NewTranscribeClient(config *config.Config, pulsarClient *pulsar.PulsarClient, s3Client *s3.S3Client, asrClient *asr.ASRClient, ollamaClient *ollama.OllamaClient, dragonflyClient *dragonfly.DragonflyClient) *TranscribeClient {
	slackClient := slack.New(config.SlackToken)

	return &TranscribeClient{
		pulsarClient:    pulsarClient,
		s3Client:        s3Client,
		asrClient:       asrClient,
		ollamaClient:    ollamaClient,
		slackClient:     slackClient,
		dragonflyClient: dragonflyClient,
		config:          config,
	}
}

func (tc *TranscribeClient) Work(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
			msg, err := tc.pulsarClient.Receive(ctx)
			if err != nil {
				slog.Error("failed to receive message from pulsar", slog.String("error", err.Error()))
				if msg != nil {
					tc.pulsarClient.Nack(msg)
				}
				continue
			}

			workCtx, workCancel := context.WithTimeout(ctx, time.Duration(tc.config.WorkerTimeout))

			slog.Debug("received message from pulsar", slog.String("message_id", msg.ID().String()))

			var eventSchema s3event.EventSchema
			if err := json.Unmarshal(msg.Payload(), &eventSchema); err != nil {
				slog.Error("failed to unmarshal S3 event", slog.String("error", err.Error()))

				tc.pulsarClient.Nack(msg)
				workCancel()
				continue
			}

			slog.Debug("unmarshalled S3 event", slog.Any("event_schema", eventSchema))

			if len(eventSchema.Records) == 0 {
				slog.Error("no records in S3 event, skipping")
				_ = tc.pulsarClient.Ack(msg)
				workCancel()
				continue
			}

			err = tc.pulsarClient.Ack(msg)
			if err != nil {
				slog.Error("failed to acknowledge message", slog.String("error", err.Error()))
			}

			slog.Debug("acknowledged message", slog.String("message_id", msg.ID().String()))

			for _, record := range eventSchema.Records {
				if record.EventName != s3event.EventObjectCreatedPut &&
					record.EventName != s3event.EventObjectCreatedPost {
					slog.Debug("skipping non-object-created event", slog.String("event_name", string(record.EventName)))
					continue
				}

				splitKey := strings.Split(record.S3.Object.Key, ".")
				if splitKey[len(splitKey)-1] != "wav" {
					slog.Debug("skipping non-wav file", slog.String("key", record.S3.Object.Key))
					continue
				}

				slog.Debug("processing S3 object", slog.String("key", record.S3.Object.Key))

				isAllowed, parsedKey, err := tc.IsObjectAllowed(workCtx, record.S3.Object.Key)
				if err != nil {
					slog.Error("failed to check if object is allowed", slog.String("error", err.Error()), slog.String("key", record.S3.Object.Key))
					continue
				}

				if !isAllowed {
					slog.Debug("object not allowed", slog.String("key", record.S3.Object.Key))
					continue
				}

				slog.Debug("object is allowed", slog.String("key", record.S3.Object.Key), slog.Any("parsed_key", parsedKey))

				fileBytes, err := tc.s3Client.GetFile(workCtx, record.S3.Object.Key)
				if err != nil {
					slog.Error("failed to get S3 file", slog.String("error", err.Error()), slog.String("key", record.S3.Object.Key))
					continue
				}

				slog.Debug("got S3 file", slog.String("key", record.S3.Object.Key))

				tr, err := tc.asrClient.Transcribe(workCtx, record.S3.Object.Key, bytes.NewBuffer(fileBytes))
				if err != nil {
					slog.Error("failed to transcribe file", slog.String("error", err.Error()), slog.String("key", record.S3.Object.Key))
					continue
				}

				slog.Info("transcription completed", slog.String("key", record.S3.Object.Key), slog.String("transcription", tr.Transcription))

				// -=-=-=-=-=-=-=-=-=-=-=-=- PROCESSING LOGIC -=-=-=-=-=-=-=-=-=-=-=-=-

				if parsedKey.dk.Talkgroup == "1399" { // Fire Dispatch
					err := tc.processDispatchCall(workCtx, parsedKey, tr)
					if err != nil {
						slog.Error("failed to process fire dispatch call", slog.String("error", err.Error()), slog.String("talkgroup", parsedKey.dk.Talkgroup), slog.String("transcription", tr.Transcription))
						continue
					}
				} else {
					err := tc.processNonDispatchCall(workCtx, parsedKey, tr)
					if err != nil {
						slog.Error("failed to process non-dispatch call", slog.String("error", err.Error()), slog.String("talkgroup", parsedKey.dk.Talkgroup), slog.String("transcription", tr.Transcription))
						continue
					}
				}
			}

			// -=-=-=-=-=-=-=-=-=-=-=-=- END PROCESSING LOGIC -=-=-=-=-=-=-=-=-=-=-=-=-

			workCancel()
		}
	}
}
