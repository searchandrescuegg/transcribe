package transcribe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
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

	// Warm the CAD unit-context cache for this rescue (best-effort, no-op when enrichment is
	// disabled). Resolving here — right after we know the incident is a trail rescue — means the
	// first TAC transmission already has a unit roster to canonicalize against, and the dispatch
	// capture time anchors incident-recency scoring. Failures are swallowed inside the helper.
	if tc.unitResolver != nil {
		tc.resolveAndCacheUnitContext(ctx, tg.TGID, tr.Transcription, parsedKey.dk.Time)
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

	// Clean the raw ASR transmission before it goes anywhere. The cleaned text is what we post
	// in the thread AND what feeds the cumulative summary. Best-effort: on any failure we fall
	// back to the raw transcription so the transmission is never dropped.
	cleaned := tc.maybeCleanTranscript(ctx, parsedKey.dk.Talkgroup, tr.Transcription)

	// FIX (review item #1): sendSlackWithRetry actually retries on rate limit; the prior path
	// waited and discarded the message. Errors now propagate so Work() can Nack for redelivery.
	if _, err := tc.sendSlackWithRetry(ctx, parsedKey.dk.Talkgroup,
		slack.MsgOptionBlocks(BuildThreadCommunicationBlocks(&ThreadCommunicationBlocksInput{
			Channel: tgInfo.FullName,
			Message: cleaned,
			TS:      time.Now().Local(),
		})...),
		slack.MsgOptionAsUser(true),
		slack.MsgOptionTS(tsThread),
	); err != nil {
		return fmt.Errorf("%w: %s", ErrFailedToPostSlackMessage, err.Error())
	}

	slog.Debug("posted transcription message to Slack", slog.String("talkgroup", parsedKey.dk.Talkgroup), slog.String("thread_id", tsThread))

	// Roll the rescue's live interpretation forward with the CLEANED text. Best-effort and
	// decoupled — if the LLM call or chat.update fails we still consider the TAC transmission
	// processed (the per-message thread reply above is the canonical record). Uses
	// parsedKey.dk.Time as the capture moment so the model sees stable timestamps even when
	// pipeline latency varies between transmissions.
	tc.updateLiveInterpretation(ctx, parsedKey.dk.Talkgroup, parsedKey.dk.Time, cleaned)
	return nil
}

// maybeCleanTranscript runs the per-transmission LLM cleanup pass, returning a corrected
// transcription. Gated by TAC_CLEANUP_ENABLED; when disabled (or on any error / empty result) it
// returns the raw text unchanged so a transmission is never lost. The dispatch transcription and
// (optional) CAD unit roster are passed as context so the model can disambiguate and canonicalize.
func (tc *TranscribeClient) maybeCleanTranscript(ctx context.Context, tgid, raw string) string {
	if !tc.config.TACCleanupEnabled {
		return raw
	}

	// Bound the whole cleanup step (CAD lookup + LLM) with its own sub-context so a slow cleanup
	// can't eat the worker budget the thread reply and live summary still need. Zero disables the
	// sub-bound and falls back to the worker context + backend per-request timeout.
	cleanCtx := ctx
	if tc.config.TACCleanupTimeout > 0 {
		var cancel context.CancelFunc
		cleanCtx, cancel = context.WithTimeout(ctx, tc.config.TACCleanupTimeout)
		defer cancel()
	}

	var dispatchText string
	if meta, ok := tc.readClosureMeta(cleanCtx, tgid); ok {
		dispatchText = meta.Transcription
	}
	unitContext := tc.unitContextFor(cleanCtx, tgid, dispatchText, time.Now())

	res, err := tc.mlClient.CleanTACTranscript(cleanCtx, ml.TACCleanupInput{
		Text:            raw,
		DispatchContext: dispatchText,
		UnitContext:     unitContext,
	})
	if err != nil {
		slog.Warn("tac cleanup failed; posting raw transcription", slog.String("error", err.Error()), slog.String("tgid", tgid))
		return raw
	}
	if res == nil || strings.TrimSpace(res.CleanedText) == "" {
		return raw
	}
	return res.CleanedText
}
