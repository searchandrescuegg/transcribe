package transcribe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/cespare/xxhash/v2"
	"github.com/searchandrescuegg/transcribe/internal/ml"
	"github.com/searchandrescuegg/transcribe/pkg/asr"
	"github.com/slack-go/slack"
)

const (
	talkgroupKeyPrefix = "tg:%s"
)

var (
	ErrFailedToParseDispatchMessage     = errors.New("failed to parse dispatch message")
	ErrFailedToFindTalkgroup            = errors.New("failed to find talkgroup for tac channel")
	ErrFailedToAddTalkgroupToAllowlist  = errors.New("failed to add talkgroup to allowed list")
	ErrFailedToGetThreadIDFromDragonfly = errors.New("failed to get thread id from dragonfly")
)

func (tc *TranscribeClient) handleSlackRateLimit(ctx context.Context, err error, talkgroup string) error {
	var rateLimited *slack.RateLimitedError
	if errors.As(err, &rateLimited) && rateLimited.Retryable() {
		slog.Warn("slack rate limited, retrying", slog.String("error", err.Error()), slog.String("talkgroup", talkgroup))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(rateLimited.RetryAfter):
			return nil
		}
	}
	return err
}

func selectTrailRescueMessage(dispatchMessages *ml.DispatchMessages, transcription string) (*ml.DispatchMessage, string) {
	// Track message hashes to detect duplicates - stores all processed messages for deduplication
	// but function returns on first valid trail rescue message found
	messageHashes := make(map[string]ml.DispatchMessage)
	for i, dispatchMessage := range dispatchMessages.Messages {
		if !CallIsTrailRescue(dispatchMessage.CallType) {
			slog.Warn("call is not a trail rescue", slog.String("call_type", dispatchMessage.CallType), slog.String("transcription", transcription))
			continue
		}

		hash := xxhash.Sum64([]byte(dispatchMessage.CleanedTranscription))
		hashStr := fmt.Sprintf("%d", hash)

		if _, exists := messageHashes[hashStr]; exists {
			slog.Warn("duplicate dispatch message detected, skipping", slog.String("call_type", dispatchMessage.CallType), slog.String("tac_channel", dispatchMessage.TACChannel), slog.Int("message_index", i+1))
			continue
		}
		messageHashes[hashStr] = dispatchMessage

		slog.Info("trail rescue call detected", slog.String("call_type", dispatchMessage.CallType), slog.String("tac_channel", dispatchMessage.TACChannel), slog.Int("message_index", i+1), slog.String("message_hash", hashStr))

		return &dispatchMessage, hashStr
	}
	return nil, ""
}

func (tc *TranscribeClient) processDispatchCall(ctx context.Context, parsedKey *AdornedDeconstructedKey, tr *asr.TranscriptionResponse) error {
	slog.Debug("processing fire dispatch transcription", slog.String("talkgroup", parsedKey.dk.Talkgroup), slog.String("transcription", tr.Transcription))

	dispatchMessages, err := tc.mlClient.ParseRelevantInformationFromDispatchMessage(tr.Transcription)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrFailedToParseDispatchMessage, err.Error())
	}

	slog.Debug("parsed dispatch messages", slog.Int("len", len(dispatchMessages.Messages)), slog.Any("dispatch_messages", dispatchMessages))

	selectedDispatchMessage, selectedMessageHash := selectTrailRescueMessage(dispatchMessages, tr.Transcription)
	if selectedDispatchMessage == nil {
		slog.Warn("no trail rescue call found in dispatch messages")
		return nil
	}

	dispatchMessage := *selectedDispatchMessage

	tg, ok := talkgroupFromRadioShortCode[dispatchMessage.TACChannel]
	if !ok {
		return fmt.Errorf("%w: %s", ErrFailedToFindTalkgroup, dispatchMessage.TACChannel)
	}

	err = tc.dragonflyClient.SAddEx(ctx, "allowed_talkgroups", tc.config.TacticalChannelActivationDuration, tg.TGID)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrFailedToAddTalkgroupToAllowlist, err.Error())
	}

	slog.Info("added TAC channel to allowed talkgroups", slog.String("tac_channel", dispatchMessage.TACChannel), slog.Any("talkgroup", tg), slog.String("message_hash", selectedMessageHash))

	expiresAt := time.Now().Add(tc.config.TacticalChannelActivationDuration).Local()

	sendMessageCtx, sendMessageCancel := context.WithTimeout(ctx, tc.config.SlackTimeout)

	_, tsThread, _, err := tc.slackClient.SendMessageContext(sendMessageCtx, tc.config.SlackChannelID, slack.MsgOptionBlocks(BuildRescueTrailBlocks(&RescueTrailBlocksInput{
		TACChannel:        dispatchMessage.TACChannel,
		TranscriptionText: tr.Transcription,
		ExpiresAt:         expiresAt,
	})...))
	if err != nil {
		if retryErr := tc.handleSlackRateLimit(ctx, err, parsedKey.dk.Talkgroup); retryErr != nil {
			if errors.Is(retryErr, context.DeadlineExceeded) || errors.Is(retryErr, context.Canceled) {
				slog.Error("context done while waiting for rate limit retry", slog.String("error", retryErr.Error()), slog.String("talkgroup", parsedKey.dk.Talkgroup))
			} else {
				slog.Error("failed to post message to Slack", slog.String("error", err.Error()))
			}
			sendMessageCancel()
		}
	}
	sendMessageCancel()

	slog.Debug("posted message to slack", slog.String("tac_channel", dispatchMessage.TACChannel), slog.String("thread_id", tsThread))

	err = tc.dragonflyClient.Set(ctx, fmt.Sprintf(talkgroupKeyPrefix, tg.TGID), tc.config.TacticalChannelActivationDuration, tsThread)
	if err != nil {
		slog.Error("failed to set TAC channel in Dragonfly", slog.String("error", err.Error()))
	}

	slog.Debug("set TAC channel in Dragonfly", slog.String("tac_channel", dispatchMessage.TACChannel), slog.String("thread_id", tsThread))

	time.AfterFunc(tc.config.TacticalChannelActivationDuration, func() {
		// Create context with timeout for cleanup operation - independent of original context
		// to ensure cleanup completes even if original request is cancelled
		afterFuncCtx, afterFuncCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer afterFuncCancel()

		slog.Debug("TAC channel expiration reached, posting channel closed message", slog.String("tac_channel", dispatchMessage.TACChannel))

		channelClosedInput := ChannelClosedBlocksInput{
			Channel:  dispatchMessage.TACChannel,
			ClosedAt: time.Now().Local(),
		}

		sendMessageCtx, sendMessageCancel := context.WithTimeout(afterFuncCtx, tc.config.SlackTimeout)

		_, _, _, err = tc.slackClient.SendMessageContext(sendMessageCtx, tc.config.SlackChannelID,
			slack.MsgOptionBlocks(BuildChannelClosedBlocks(&channelClosedInput)...),
			slack.MsgOptionAsUser(true),
			slack.MsgOptionTS(tsThread),
			slack.MsgOptionBroadcast(),
		)
		if err != nil {
			if retryErr := tc.handleSlackRateLimit(afterFuncCtx, err, parsedKey.dk.Talkgroup); retryErr != nil {
				if errors.Is(retryErr, context.DeadlineExceeded) || errors.Is(retryErr, context.Canceled) {
					slog.Error("context done while waiting for rate limit retry", slog.String("error", retryErr.Error()), slog.String("talkgroup", parsedKey.dk.Talkgroup))
				} else {
					slog.Error("failed to post channel closed message to Slack", slog.String("error", err.Error()), slog.String("tac_channel", dispatchMessage.TACChannel))
				}
				sendMessageCancel()
				return
			}
		}
		sendMessageCancel()

		slog.Debug("posted channel closed message to Slack", slog.String("tac_channel", dispatchMessage.TACChannel), slog.String("thread_id", tsThread))
	},
	)

	return nil
}

func (tc *TranscribeClient) processNonDispatchCall(ctx context.Context, parsedKey *AdornedDeconstructedKey, tr *asr.TranscriptionResponse) error {
	slog.Debug("call is not a fire dispatch", slog.String("talkgroup", parsedKey.dk.Talkgroup), slog.String("transcription", tr.Transcription))

	// get slack thread ID from Dragonfly
	tsThread, err := tc.dragonflyClient.Get(ctx, fmt.Sprintf(talkgroupKeyPrefix, parsedKey.dk.Talkgroup))
	if err != nil {
		return fmt.Errorf("%w: %s", ErrFailedToGetThreadIDFromDragonfly, err.Error())
	}

	if tsThread == "" {
		return fmt.Errorf("%w: %s", ErrFailedToGetThreadIDFromDragonfly, "thread ID is empty")
	}

	slog.Debug("found thread ID for talkgroup", slog.String("talkgroup", parsedKey.dk.Talkgroup), slog.String("thread_id", tsThread))

	tgInfo, ok := talkgroupFromTGID[parsedKey.dk.Talkgroup]
	if !ok {
		return fmt.Errorf("%w: %s", ErrFailedToFindTalkgroup, parsedKey.dk.Talkgroup)
	}

	slog.Debug("found talkgroup information", slog.String("talkgroup", parsedKey.dk.Talkgroup), slog.Any("talkgroup_info", tgInfo))

	sendMessageCtx, sendMessageCancel := context.WithTimeout(ctx, tc.config.SlackTimeout)

	_, _, _, err = tc.slackClient.SendMessageContext(sendMessageCtx, tc.config.SlackChannelID,
		slack.MsgOptionBlocks(BuildThreadCommunicationBlocks(&ThreadCommunicationBlocksInput{
			Channel: tgInfo.FullName,
			Message: tr.Transcription,
			TS:      time.Now().Local(),
		})...),
		slack.MsgOptionAsUser(true),
		slack.MsgOptionTS(tsThread),
	)
	if err != nil {
		if retryErr := tc.handleSlackRateLimit(ctx, err, parsedKey.dk.Talkgroup); retryErr != nil {
			if errors.Is(retryErr, context.DeadlineExceeded) || errors.Is(retryErr, context.Canceled) {
				slog.Error("context done while waiting for rate limit retry", slog.String("error", retryErr.Error()), slog.String("talkgroup", parsedKey.dk.Talkgroup))
				sendMessageCancel()
				return retryErr
			} else {
				slog.Error("failed to post transcription message to Slack", slog.String("error", err.Error()))
				sendMessageCancel()
			}
		}
	}
	sendMessageCancel()

	slog.Debug("posted transcription message to Slack", slog.String("talkgroup", parsedKey.dk.Talkgroup), slog.String("thread_id", tsThread))
	return nil
}
