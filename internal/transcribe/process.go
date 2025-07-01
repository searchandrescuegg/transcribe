package transcribe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/searchandrescuegg/transcribe/pkg/asr"
	"github.com/slack-go/slack"
)

var (
	ErrFailedToParseDispatchMessage     = errors.New("failed to parse dispatch message")
	ErrFailedToFindTalkgroup            = errors.New("failed to find talkgroup for tac channel")
	ErrFailedToAddTalkgroupToAllowlist  = errors.New("failed to add talkgroup to allowed list")
	ErrFailedToGetThreadIDFromDragonfly = errors.New("failed to get thread id from dragonfly")
)

func (tc *TranscribeClient) processDispatchCall(ctx context.Context, parsedKey *AdornedDeconstructedKey, tr *asr.TranscriptionResponse) error {
	slog.Debug("processing fire dispatch transcription", slog.String("talkgroup", parsedKey.dk.Talkgroup), slog.String("transcription", tr.Transcription))

	dispatchMessage, err := tc.ollamaClient.ParseRelevantInformationFromDispatchMessage(tr.Transcription)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrFailedToParseDispatchMessage, err.Error())
	}

	slog.Debug("parsed dispatch message", slog.Any("dispatch_message", dispatchMessage))

	if !CallIsTrailRescue(dispatchMessage.CallType) {
		slog.Warn("call is not a trail rescue", slog.String("call_type", dispatchMessage.CallType), slog.String("transcription", tr.Transcription))
		return nil
	}

	slog.Info("trail rescue call detected", slog.String("call_type", dispatchMessage.CallType), slog.String("tac_channel", dispatchMessage.TACChannel))

	tg, ok := talkgroupFromRadioShortCode[dispatchMessage.TACChannel]
	if !ok {
		return fmt.Errorf("%w: %s", ErrFailedToFindTalkgroup, dispatchMessage.TACChannel)
	}

	err = tc.dragonflyClient.SAddEx(ctx, "allowed_talkgroups", tc.config.TacticalChannelActivationDuration, tg.TGID)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrFailedToAddTalkgroupToAllowlist, err.Error())
	}

	slog.Info("added TAC channel to allowed talkgroups", slog.String("tac_channel", dispatchMessage.TACChannel), slog.Any("talkgroup", tg))

	expiresAt := time.Now().Add(tc.config.TacticalChannelActivationDuration).Local()

	sendMessageCtx, sendMessageCancel := context.WithTimeout(ctx, tc.config.SlackTimeout)

	_, tsThread, _, err := tc.slackClient.SendMessageContext(sendMessageCtx, tc.config.SlackChannelID, slack.MsgOptionBlocks(BuildRescueTrailBlocks(&RescueTrailBlocksInput{
		TACChannel:        dispatchMessage.TACChannel,
		TranscriptionText: tr.Transcription,
		ExpiresAt:         expiresAt,
	})...))
	if err != nil {
		var rateLimited *slack.RateLimitedError
		if errors.As(err, &rateLimited) && rateLimited.Retryable() {
			slog.Warn("slack rate limited, retrying", slog.String("error", err.Error()), slog.String("talkgroup", parsedKey.dk.Talkgroup))
			select {
			case <-ctx.Done():
				err = ctx.Err()
				slog.Error("context done while waiting for rate limit retry", slog.String("error", err.Error()), slog.String("talkgroup", parsedKey.dk.Talkgroup))
			case <-time.After(rateLimited.RetryAfter):
				err = nil
			}
		} else {
			sendMessageCancel()
			slog.Error("failed to post message to Slack", slog.String("error", err.Error()))
		}
	}
	sendMessageCancel()

	slog.Debug("posted message to slack", slog.String("tac_channel", dispatchMessage.TACChannel), slog.String("thread_id", tsThread))

	err = tc.dragonflyClient.Set(ctx, fmt.Sprintf("tg:%s", tg.TGID), tc.config.TacticalChannelActivationDuration, tsThread)
	if err != nil {
		slog.Error("failed to set TAC channel in Dragonfly", slog.String("error", err.Error()))
	}

	slog.Debug("set TAC channel in Dragonfly", slog.String("tac_channel", dispatchMessage.TACChannel), slog.String("thread_id", tsThread))

	time.AfterFunc(tc.config.TacticalChannelActivationDuration, func() {
		afterFuncCtx := context.Background()

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
			var rateLimited *slack.RateLimitedError
			if errors.As(err, &rateLimited) && rateLimited.Retryable() {
				slog.Warn("slack rate limited, retrying", slog.String("error", err.Error()), slog.String("talkgroup", parsedKey.dk.Talkgroup))
				select {
				case <-ctx.Done():
					err = ctx.Err()
					slog.Error("context done while waiting for rate limit retry", slog.String("error", err.Error()), slog.String("talkgroup", parsedKey.dk.Talkgroup))
					sendMessageCancel()
					return
				case <-time.After(rateLimited.RetryAfter):
					err = nil
				}
			} else {
				slog.Error("failed to post channel closed message to Slack", slog.String("error", err.Error()), slog.String("tac_channel", dispatchMessage.TACChannel))
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
	tsThread, err := tc.dragonflyClient.Get(ctx, fmt.Sprintf("tg:%s", parsedKey.dk.Talkgroup))
	if err != nil {
		return fmt.Errorf("%w: %s", ErrFailedToGetThreadIDFromDragonfly, err.Error())
	}

	slog.Debug("got thread ID from Dragonfly", slog.String("talkgroup", parsedKey.dk.Talkgroup), slog.String("thread_id", tsThread))

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
		var rateLimited *slack.RateLimitedError
		if errors.As(err, &rateLimited) && rateLimited.Retryable() {
			slog.Warn("slack rate limited, retrying", slog.String("error", err.Error()), slog.String("talkgroup", parsedKey.dk.Talkgroup))
			select {
			case <-ctx.Done():
				err = ctx.Err()
				slog.Error("context done while waiting for rate limit retry", slog.String("error", err.Error()), slog.String("talkgroup", parsedKey.dk.Talkgroup))
				sendMessageCancel()
				return err
			case <-time.After(rateLimited.RetryAfter):
				err = nil
			}
		} else {
			sendMessageCancel()

		}
	}
	sendMessageCancel()

	slog.Debug("posted transcription message to Slack", slog.String("talkgroup", parsedKey.dk.Talkgroup), slog.String("thread_id", tsThread))
	return nil
}
