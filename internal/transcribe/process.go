package transcribe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/cespare/xxhash/v2"
	"github.com/searchandrescuegg/transcribe/internal/asr"
	"github.com/searchandrescuegg/transcribe/internal/ml"
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
	ErrFailedToPostSlackMessage         = errors.New("failed to post slack message")
)

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

	// FIX (review item #3): pass ctx through so worker timeout / shutdown actually cancels the LLM call.
	dispatchMessages, err := tc.mlClient.ParseRelevantInformationFromDispatchMessage(ctx, tr.Transcription)
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

	// FIX (review item #1): sendSlackWithRetry actually retries after RetryAfter on 429s,
	// where the previous handleSlackRateLimit waited and silently dropped the message.
	// FIX (review item #1, follow-on): if the post fails entirely, we now bail out instead of
	// continuing to schedule a TAC closure against an empty thread_ts.
	tsThread, err := tc.sendSlackWithRetry(ctx, parsedKey.dk.Talkgroup,
		slack.MsgOptionBlocks(BuildRescueTrailBlocks(&RescueTrailBlocksInput{
			TACChannel:        dispatchMessage.TACChannel,
			TranscriptionText: tr.Transcription,
			ExpiresAt:         expiresAt,
			DispatchTGID:      FireDispatch1TGID,
			TACTalkgroupTGID:  tg.TGID, // enables the slackctl controller's Cancel/Extend buttons
		})...))
	if err != nil {
		return fmt.Errorf("%w: %s", ErrFailedToPostSlackMessage, err.Error())
	}

	slog.Debug("posted message to slack", slog.String("tac_channel", dispatchMessage.TACChannel), slog.String("thread_id", tsThread))

	err = tc.dragonflyClient.Set(ctx, fmt.Sprintf(talkgroupKeyPrefix, tg.TGID), tc.config.TacticalChannelActivationDuration, tsThread)
	if err != nil {
		slog.Error("failed to set TAC channel in Dragonfly", slog.String("error", err.Error()))
	}

	slog.Debug("set TAC channel in Dragonfly", slog.String("tac_channel", dispatchMessage.TACChannel), slog.String("thread_id", tsThread))

	// FIX (review item #10 / option B): persisted ZSET entry replaces in-process time.AfterFunc.
	// Previously a process restart would silently drop the scheduled "channel closed" Slack message;
	// the sweeper goroutine now picks it up after restart based on the recorded expiry.
	if err := tc.ScheduleTACClosure(ctx, ClosureMeta{
		TGID:            tg.TGID,
		TACChannel:      dispatchMessage.TACChannel,
		ThreadTS:        tsThread,
		SourceTalkgroup: parsedKey.dk.Talkgroup,
		MessageTS:       tsThread, // alert is the thread parent; ts == thread_ts for chat.update later
		Transcription:   tr.Transcription,
	}, expiresAt); err != nil {
		slog.Error("failed to persist TAC closure schedule", slog.String("error", err.Error()), slog.String("tac_channel", dispatchMessage.TACChannel))
	}

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

	// FIX (review item #1): sendSlackWithRetry actually retries on rate limit; the prior path
	// waited and discarded the message. Errors now propagate so Work() can Nack for redelivery.
	if _, err := tc.sendSlackWithRetry(ctx, parsedKey.dk.Talkgroup,
		slack.MsgOptionBlocks(BuildThreadCommunicationBlocks(&ThreadCommunicationBlocksInput{
			Channel: tgInfo.FullName,
			Message: tr.Transcription,
			TS:      time.Now().Local(),
		})...),
		slack.MsgOptionAsUser(true),
		slack.MsgOptionTS(tsThread),
	); err != nil {
		return fmt.Errorf("%w: %s", ErrFailedToPostSlackMessage, err.Error())
	}

	slog.Debug("posted transcription message to Slack", slog.String("talkgroup", parsedKey.dk.Talkgroup), slog.String("thread_id", tsThread))

	// Roll the rescue's live interpretation forward. Best-effort and decoupled — if the
	// LLM call or chat.update fails we still consider the TAC transmission processed (the
	// per-message thread reply above is the canonical record). Uses parsedKey.dk.Time as
	// the capture moment so the model sees stable timestamps even when pipeline latency
	// varies between transmissions.
	tc.updateLiveInterpretation(ctx, parsedKey.dk.Talkgroup, parsedKey.dk.Time, tr.Transcription)
	return nil
}
