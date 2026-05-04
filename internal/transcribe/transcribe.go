package transcribe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	pulsarapi "github.com/apache/pulsar-client-go/pulsar"
	"github.com/searchandrescuegg/transcribe/internal/asr"
	"github.com/searchandrescuegg/transcribe/internal/config"
	"github.com/searchandrescuegg/transcribe/internal/dragonfly"
	"github.com/searchandrescuegg/transcribe/internal/ml"
	"github.com/searchandrescuegg/transcribe/internal/pulsar"
	"github.com/searchandrescuegg/transcribe/internal/s3"
	"github.com/slack-go/slack"
	"github.com/versity/versitygw/s3event"
)

const (
	dedupKeyPrefix = "dedup:%s"

	// dispatchInFlightKey is set by the worker that picks up a Fire Dispatch 1 (1399)
	// event before it does any other work. Workers that pick up a TAC event and find it
	// not-allowed read this key — if set, they nack for Pulsar redelivery instead of
	// silently ack-and-dropping. This recovers the production race where a TAC channel
	// starts transmitting within seconds of dispatch but the dispatch's ML round-trip
	// (ASR + LLM, ~10-30s) hasn't finished updating allowed_talkgroups yet.
	//
	// TTL is the WorkerTimeout — outlasts the worker's full S3+ASR+ML+Slack processing
	// budget. After that, a still-rejected TAC is genuinely off-incident traffic and
	// reverts to ack-and-drop.
	dispatchInFlightKey = "dispatch_in_flight"
)

// FIX (review item #20): SlackPoster lets tests substitute a mock without bringing in the
// real slack-go client. The real *slack.Client satisfies this implicitly via structural
// subtyping, so production wiring is unchanged.
//
// UpdateMessageContext lets the sweeper rewrite the parent rescue alert when the auto-close
// fires (remove the actions block, change "expires" to "auto-closed"). The signature mirrors
// the slack-go method exactly so *slack.Client continues to satisfy the interface.
type SlackPoster interface {
	SendMessageContext(ctx context.Context, channelID string, options ...slack.MsgOption) (string, string, string, error)
	UpdateMessageContext(ctx context.Context, channelID, timestamp string, options ...slack.MsgOption) (string, string, string, error)
}

// MLClient bundles the two ML capabilities the worker uses: the dispatch-parser for
// turning a raw transcription into structured trail-rescue info, and the summarizer that
// turns dispatch + ordered TAC transmissions into a structured situational summary.
// *openai.OpenAIClient implements both, so production wiring stays a single dependency.
type MLClient interface {
	ml.DispatchMessageParser
	ml.RescueSummarizer
}

type TranscribeClient struct {
	pulsarClient    *pulsar.PulsarClient
	s3Client        *s3.S3Client
	asrClient       *asr.ASRClient
	mlClient        MLClient
	slackClient     SlackPoster
	dragonflyClient *dragonfly.DragonflyClient

	config *config.Config
}

func NewTranscribeClient(config *config.Config, pulsarClient *pulsar.PulsarClient, s3Client *s3.S3Client, asrClient *asr.ASRClient, mlClient MLClient, dragonflyClient *dragonfly.DragonflyClient) *TranscribeClient {
	return &TranscribeClient{
		pulsarClient:    pulsarClient,
		s3Client:        s3Client,
		asrClient:       asrClient,
		mlClient:        mlClient,
		slackClient:     slack.New(config.SlackToken),
		dragonflyClient: dragonflyClient,
		config:          config,
	}
}

// newTranscribeClientForTest is a test-only constructor that accepts a SlackPoster directly.
// Avoids the production NewTranscribeClient's hard-coded slack.New(token) so unit tests can
// inject a testify mock.
func newTranscribeClientForTest(c *config.Config, pulsarClient *pulsar.PulsarClient, s3Client *s3.S3Client, asrClient *asr.ASRClient, mlClient MLClient, slackClient SlackPoster, dragonflyClient *dragonfly.DragonflyClient) *TranscribeClient {
	return &TranscribeClient{
		pulsarClient:    pulsarClient,
		s3Client:        s3Client,
		asrClient:       asrClient,
		mlClient:        mlClient,
		slackClient:     slackClient,
		dragonflyClient: dragonflyClient,
		config:          c,
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

			tc.handleMessage(ctx, msg)
		}
	}
}

// handleMessage owns the lifecycle of a single Pulsar message.
//
// FIX (review item #2): the previous implementation called Ack immediately after unmarshal,
// before any record was processed. Any failure during transcription / ML / Slack therefore
// silently lost the audio. We now Ack only after every record's processRecord has completed
// (success OR an idempotent "already-processed" skip), and Nack on any unexpected error so
// Pulsar redelivers the message.
func (tc *TranscribeClient) handleMessage(ctx context.Context, msg pulsarapi.Message) {
	// FIX (review item #25): tc.config.WorkerTimeout is already a time.Duration; the prior
	// time.Duration(...) cast was a no-op and has been removed.
	workCtx, workCancel := context.WithTimeout(ctx, tc.config.WorkerTimeout)
	defer workCancel()

	slog.Debug("received message from pulsar", slog.String("message_id", msg.ID().String()))

	var eventSchema s3event.EventSchema
	if err := json.Unmarshal(msg.Payload(), &eventSchema); err != nil {
		slog.Error("failed to unmarshal S3 event, nacking", slog.String("error", err.Error()))
		tc.pulsarClient.Nack(msg)
		return
	}

	slog.Debug("unmarshalled S3 event", slog.Any("event_schema", eventSchema))

	if len(eventSchema.Records) == 0 {
		slog.Warn("no records in S3 event, acking and skipping")
		_ = tc.pulsarClient.Ack(msg)
		return
	}

	allSucceeded := true
	for _, record := range eventSchema.Records {
		if err := tc.processRecord(workCtx, &record); err != nil {
			slog.Error("failed to process S3 record", slog.String("error", err.Error()), slog.String("key", record.S3.Object.Key))
			allSucceeded = false
		}
	}

	// FIX (review item #2): only Ack if we made it through the records cleanly. On any error,
	// Nack so Pulsar redelivers — the dedup guard below ensures the retry is idempotent.
	if !allSucceeded {
		tc.pulsarClient.Nack(msg)
		return
	}
	if err := tc.pulsarClient.Ack(msg); err != nil {
		slog.Error("failed to ack message", slog.String("error", err.Error()), slog.String("message_id", msg.ID().String()))
	}
}

// processRecord handles a single S3 event record from the Pulsar message.
// Extracted from Work() so the ack/nack policy lives in one place and the per-record flow
// is independently testable.
func (tc *TranscribeClient) processRecord(ctx context.Context, record *s3event.EventRecord) error {
	if record.EventName != s3event.EventObjectCreatedPut &&
		record.EventName != s3event.EventObjectCreatedPost {
		slog.Debug("skipping non-object-created event", slog.String("event_name", string(record.EventName)))
		return nil
	}

	key := record.S3.Object.Key

	// FIX (review item #27): HasSuffix is clearer than split-and-index for a suffix check.
	if !strings.HasSuffix(key, ".wav") {
		slog.Debug("skipping non-wav file", slog.String("key", key))
		return nil
	}

	slog.Debug("processing S3 object", slog.String("key", key))

	isAllowed, parsedKey, err := tc.IsObjectAllowed(ctx, key)
	if err != nil {
		return fmt.Errorf("failed to check if object is allowed: %w", err)
	}
	if !isAllowed {
		// FIX (race recovery): if a dispatch is currently being processed, this rejection
		// might just be "TAC arrived before allow-list updated". Return an error so handleMessage
		// nacks the message for Pulsar redelivery (NackRedeliveryDelay=30s by default). After
		// the dispatch finishes its ML round-trip and the TAC lands in allowed_talkgroups, the
		// redelivered copy will pass IsObjectAllowed. With no dispatch in flight, the rejection
		// is real off-incident traffic — keep the original ack-and-drop behavior so we don't
		// turn the steady-state into a redelivery storm.
		if marker, _ := tc.dragonflyClient.Get(ctx, dispatchInFlightKey); marker == "1" {
			return fmt.Errorf("rejected during in-flight dispatch (talkgroup=%s); nacking for Pulsar redelivery", parsedKey.dk.Talkgroup)
		}
		slog.Debug("object not allowed", slog.String("key", key))
		return nil
	}

	slog.Debug("object is allowed", slog.String("key", key), slog.Any("parsed_key", parsedKey))

	// FIX (review item #11): SETNX-based dedup guard against Pulsar redelivery.
	//
	// FIX (race against allow-list timing): the dedup-set MUST happen AFTER IsObjectAllowed
	// returns true. Earlier placement burned a dedup slot for every rejected message — so
	// when a TAC transmission arrived seconds before its dispatch had finished ML-classifying
	// (real production race: ASR + LLM round-trip can be ~10-30s, but TAC traffic can start
	// within seconds of dispatch), the message was rejected, the dedup key was set, and any
	// re-publish or redelivery for the same audio was then silently skipped — even after the
	// channel made it onto the allow-list. By dedup'ing only on messages we'd actually do
	// work on, a redelivered TAC transmission gets a second chance once its channel is live.
	//
	// Acquired==false means another worker has already started this key — non-error skip so
	// the outer Pulsar message can still be acked.
	acquired, err := tc.dragonflyClient.SetNX(ctx, fmt.Sprintf(dedupKeyPrefix, key), tc.config.DedupTTL, "1")
	if err != nil {
		// Don't fail the whole record on a dedup-store hiccup; log and continue best-effort.
		slog.Warn("dedup SETNX failed, processing anyway", slog.String("error", err.Error()), slog.String("key", key))
	} else if !acquired {
		slog.Info("skipping already-processed object (dedup hit)", slog.String("key", key))
		return nil
	}

	// FIX (dispatch-in-flight + dedup interaction): the marker MUST be set after the
	// dedup check, not before. An early stamp combined with a stale dedup key (operator
	// re-runs the synthetic trigger; Pulsar redelivers a still-valid message) caused the
	// dispatch to dedup-skip while leaving the marker set — every TAC transmission for the
	// next WorkerTimeout window would then nack-for-retry chasing an allow-list write that
	// was never going to happen, eventually DLQ'ing them all. Setting it here means:
	// "we're definitely about to do real work for a 1399 dispatch — racing TAC events,
	// recover via redelivery." The race window between this set and the LLM finishing is
	// the same ~10-30s as before; we just lose the few microseconds between parseKey and
	// the dedup check, which never mattered for race recovery.
	if parsedKey.dk.Talkgroup == FireDispatch1TGID {
		if err := tc.dragonflyClient.Set(ctx, dispatchInFlightKey, tc.config.WorkerTimeout, "1"); err != nil {
			slog.Warn("failed to set dispatch_in_flight marker; racing TAC events will not recover", slog.String("error", err.Error()))
		}
	}

	fileBytes, err := tc.s3Client.GetFile(ctx, key)
	if err != nil {
		return fmt.Errorf("failed to get S3 file: %w", err)
	}
	slog.Debug("got S3 file", slog.String("key", key))

	tr, err := tc.asrClient.Transcribe(ctx, key, bytes.NewBuffer(fileBytes))
	if err != nil {
		return fmt.Errorf("failed to transcribe file: %w", err)
	}
	slog.Info("transcription completed", slog.String("key", key), slog.String("transcription", tr.Transcription), slog.Bool("no_speech", tr.NoSpeechDetected))

	// Radio captures often include squelch clicks, brief tones, or empty cuts. The ASR
	// flags these via NoSpeechDetected (or returns an empty body). Treat them as a clean
	// no-op rather than a failure — sending an empty string downstream would just trip
	// the OpenAI client's "transcription cannot be empty" guard and Nack the message
	// for nothing.
	if tr.NoSpeechDetected || strings.TrimSpace(tr.Transcription) == "" {
		slog.Info("skipping audio with no detectable speech", slog.String("key", key))
		return nil
	}

	if parsedKey.dk.Talkgroup == FireDispatch1TGID {
		if err := tc.processDispatchCall(ctx, parsedKey, tr); err != nil {
			return fmt.Errorf("failed to process fire dispatch call (talkgroup=%s): %w", parsedKey.dk.Talkgroup, err)
		}
		return nil
	}

	if err := tc.processNonDispatchCall(ctx, parsedKey, tr); err != nil {
		return fmt.Errorf("failed to process non-dispatch call (talkgroup=%s): %w", parsedKey.dk.Talkgroup, err)
	}
	return nil
}
