package transcribe

import (
	"context"
	"encoding/json"
	"fmt"
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
		pulsarClient: pulsarClient,
		s3Client:     s3Client,
		asrClient:    asrClient,
		ollamaClient: ollamaClient,
		slackClient:  slackClient,
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
				continue
			}

			var eventSchema s3event.EventSchema
			if err := json.Unmarshal(msg.Payload(), &eventSchema); err != nil {
				slog.Error("failed to unmarshal S3 event", slog.String("error", err.Error()))

				tc.pulsarClient.Nack(msg)
				continue
			}

			for _, record := range eventSchema.Records {
				if record.EventName != s3event.EventObjectCreatedPut &&
					record.EventName != s3event.EventObjectCreatedPost {
					// fmt.Printf("Skipping event: %s\n", record.EventName)
					continue
				}

				splitKey := strings.Split(record.S3.Object.Key, ".")
				if splitKey[len(splitKey)-1] != "wav" {
					slog.Debug("skipping non-wav file", slog.String("key", record.S3.Object.Key))
					continue
				}

				isAllowed, parsedKey, err := tc.IsObjectAllowed(ctx, record.S3.Object.Key)
				if err != nil {
					slog.Error("failed to check if object is allowed", slog.String("error", err.Error()), slog.String("key", record.S3.Object.Key))
					continue
				}

				if !isAllowed {
					slog.Debug("object not allowed", slog.String("key", record.S3.Object.Key))
					continue
				}

				fileCloser, err := tc.s3Client.GetFile(ctx, record.S3.Object.Key)
				if err != nil {
					slog.Error("failed to get S3 file", slog.String("error", err.Error()), slog.String("key", record.S3.Object.Key))
					continue
				}

				tr, err := tc.asrClient.Transcribe(ctx, record.S3.Object.Key, fileCloser)
				if err != nil {
					slog.Error("failed to transcribe file", slog.String("error", err.Error()), slog.String("key", record.S3.Object.Key))
					continue
				}

				slog.Info("transcription completed", slog.String("key", record.S3.Object.Key), slog.String("transcription", tr.Transcription))

				if parsedKey.dk.Talkgroup == "1399" { // Fire Dispatch
					dispatchMessage, err := tc.ollamaClient.ParseRelevantInformationFromDispatchMessage(tr.Transcription)
					if err != nil {
						slog.Error("failed to parse relevant information from dispatch message", slog.String("error", err.Error()), slog.String("transcription", tr.Transcription))
						continue
					}

					if !CallIsTrailRescue(dispatchMessage.CallType) {
						slog.Debug("call is not a trail rescue", slog.String("call_type", dispatchMessage.CallType))
						continue
					}

					slog.Info("trail rescue call detected", slog.String("call_type", dispatchMessage.CallType), slog.String("tac_channel", dispatchMessage.TACChannel))
					tg, ok := talkgroupFromRadioShortCode[dispatchMessage.TACChannel]
					if !ok {
						slog.Error("failed to find talkgroup for TAC channel", slog.String("tac_channel", dispatchMessage.TACChannel))
						continue
					}

					err = tc.dragonflyClient.SAddEx(ctx, "allowed_talkgroups", dragonfly.DefaultExpiration, tg)
					if err != nil {
						slog.Error("failed to add TAC channel to allowed talkgroups", slog.String("error", err.Error()), slog.String("tac_channel", dispatchMessage.TACChannel), slog.Any("talkgroup", tg))
						continue
					}

					slog.Info("added TAC channel to allowed talkgroups", slog.String("tac_channel", dispatchMessage.TACChannel), slog.Any("talkgroup", tg))
					expiresAt := time.Now().Add(dragonfly.DefaultExpiration).Local()

					_, tsThread, _, err := tc.slackClient.SendMessageContext(ctx, tc.config.SlackChannelID, slack.MsgOptionBlocks(BuildRescueTrailBlocks(&RescueTrailBlocksInput{
						TACChannel:        dispatchMessage.TACChannel,
						TranscriptionText: tr.Transcription,
						ExpiresAt:         expiresAt,
					})...))
					if err != nil {
						slog.Error("failed to post message to Slack", slog.String("error", err.Error()))
					}

					err = tc.dragonflyClient.Set(ctx, fmt.Sprintf("tg:%s", tg.TGID), dragonfly.DefaultExpiration, tsThread)
					if err != nil {
						slog.Error("failed to set TAC channel in Dragonfly", slog.String("error", err.Error()))
					}

					time.AfterFunc(dragonfly.DefaultExpiration, func() {
						channelClosedInput := ChannelClosedBlocksInput{
							Channel:  dispatchMessage.TACChannel,
							ClosedAt: time.Now().Local(),
						}

						_, _, _, err = tc.slackClient.SendMessageContext(ctx, "C09363Q926L",
							slack.MsgOptionBlocks(BuildChannelClosedBlocks(&channelClosedInput)...),
							slack.MsgOptionAsUser(true),
							slack.MsgOptionTS(tsThread),
							slack.MsgOptionBroadcast(),
						)
						if err != nil {
							fmt.Printf("Error posting channel closed message: %v\n", err)
							return
						}
					},
					)

				} else {
					slog.Debug("call is not a fire dispatch", slog.String("talkgroup", parsedKey.dk.Talkgroup), slog.String("transcription", tr.Transcription))

					// get slack thread ID from Dragonfly
					tsThread, err := tc.dragonflyClient.Get(ctx, fmt.Sprintf("tg:%s", parsedKey.dk.Talkgroup))
					if err != nil {
						slog.Error("failed to get thread ID from Dragonfly", slog.String("error", err.Error()), slog.String("talkgroup", parsedKey.dk.Talkgroup))
						continue
					}

					if tsThread == "" {
						slog.Debug("no thread ID found for talkgroup", slog.String("talkgroup", parsedKey.dk.Talkgroup))
						continue
					}
					slog.Debug("found thread ID for talkgroup", slog.String("talkgroup", parsedKey.dk.Talkgroup), slog.String("thread_id", tsThread))

					tgInfo, ok := talkgroupFromTGID[parsedKey.dk.Talkgroup]
					if !ok {
						slog.Error("failed to find talkgroup information", slog.String("talkgroup", parsedKey.dk.Talkgroup))
						continue
					}

					_, _, _, err = tc.slackClient.SendMessageContext(ctx, tc.config.SlackChannelID,
						slack.MsgOptionBlocks(BuildThreadCommunicationBlocks(&ThreadCommunicationBlocksInput{
							Channel: tgInfo.FullName,
							Message: tr.Transcription,
							TS:      time.Now().Local(),
						})...),
						slack.MsgOptionAsUser(true),
						slack.MsgOptionTS(tsThread),
					)
					if err != nil {
						slog.Error("failed to post transcription message to Slack", slog.String("error", err.Error()), slog.String("talkgroup", parsedKey.dk.Talkgroup))
					}

				}
			}

			err = tc.pulsarClient.Ack(msg)
			if err != nil {
				slog.Error("failed to acknowledge message", slog.String("error", err.Error()))
			}

		}
	}
}
