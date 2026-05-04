package transcribe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	pulsarapi "github.com/apache/pulsar-client-go/pulsar"
	"github.com/redis/go-redis/v9"
	"github.com/searchandrescuegg/transcribe/internal/asr"
	"github.com/searchandrescuegg/transcribe/internal/config"
	"github.com/searchandrescuegg/transcribe/internal/dragonfly"
	"github.com/searchandrescuegg/transcribe/internal/ml"
	internalpulsar "github.com/searchandrescuegg/transcribe/internal/pulsar"
	"github.com/slack-go/slack"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/suite"
	"github.com/testcontainers/testcontainers-go"
	pulsarmodule "github.com/testcontainers/testcontainers-go/modules/pulsar"
	"github.com/testcontainers/testcontainers-go/wait"
	"github.com/versity/versitygw/s3event"
)

// FIX (review item #20): integration tests for processDispatchCall, processNonDispatchCall,
// Sweep, and handleMessage. Run against a real Dragonfly (via testcontainers, exercising the
// actual SETNX/ZADD/ZREM semantics we depend on) and a real Pulsar (for the handleMessage
// ack/nack lifecycle). Slack and the ML client are testify/mock — for those the value is
// asserting *what we sent*, not that the wire format parses.

// ============================================================================
// Mocks
// ============================================================================

type mockSlackPoster struct {
	mock.Mock
}

func (m *mockSlackPoster) SendMessageContext(ctx context.Context, channelID string, options ...slack.MsgOption) (string, string, string, error) {
	args := m.Called(ctx, channelID, options)
	return args.String(0), args.String(1), args.String(2), args.Error(3)
}

func (m *mockSlackPoster) UpdateMessageContext(ctx context.Context, channelID, timestamp string, options ...slack.MsgOption) (string, string, string, error) {
	args := m.Called(ctx, channelID, timestamp, options)
	return args.String(0), args.String(1), args.String(2), args.Error(3)
}

type mockMLClient struct {
	mock.Mock
}

func (m *mockMLClient) ParseRelevantInformationFromDispatchMessage(ctx context.Context, transcription string) (*ml.DispatchMessages, error) {
	args := m.Called(ctx, transcription)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*ml.DispatchMessages), args.Error(1)
}

func (m *mockMLClient) SummarizeRescue(ctx context.Context, input ml.RescueSummaryInput) (*ml.RescueSummary, error) {
	args := m.Called(ctx, input)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*ml.RescueSummary), args.Error(1)
}

// ============================================================================
// Suite
// ============================================================================

type DispatchSuite struct {
	suite.Suite

	ctx context.Context

	dragonflyContainer testcontainers.Container
	pulsarContainer    *pulsarmodule.Container

	redisAddr string
	pulsarURL string

	// Reusable raw redis client for state inspection / FlushDB between tests.
	rdb *redis.Client
}

func TestDispatchSuite(t *testing.T) {
	suite.Run(t, new(DispatchSuite))
}

func (s *DispatchSuite) SetupSuite() {
	s.ctx = context.Background()

	// CLAUDE.md invariant #7: tests that depend on display formatting set time.Local
	// themselves. CI containers default to UTC, but feedback_test asserts a PDT-formatted
	// "MST"-suffixed string, so pin the suite to America/Los_Angeles.
	loc, err := time.LoadLocation("America/Los_Angeles")
	s.Require().NoError(err, "load America/Los_Angeles (binary embeds time/tzdata)")
	time.Local = loc

	// Dragonfly speaks the Redis protocol; we use the canonical image so the suite tests the
	// same product running in prod (not a redis-server stand-in that may diverge on edge cases).
	dfReq := testcontainers.ContainerRequest{
		Image:        "docker.dragonflydb.io/dragonflydb/dragonfly:latest",
		ExposedPorts: []string{"6379/tcp"},
		// Constrain memory + threads so the test container can co-exist with a running
		// docker-compose Dragonfly. Without these, Dragonfly auto-sizes to ~80% of host
		// memory and refuses to start when something else is already consuming it.
		Cmd: []string{
			"--proactor_threads=2",
			"--maxmemory=512mb",
		},
		WaitingFor: wait.ForListeningPort("6379/tcp").WithStartupTimeout(60 * time.Second),
	}
	df, err := testcontainers.GenericContainer(s.ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: dfReq,
		Started:          true,
	})
	s.Require().NoError(err, "start dragonfly container")
	s.dragonflyContainer = df

	host, err := df.Host(s.ctx)
	s.Require().NoError(err)
	port, err := df.MappedPort(s.ctx, "6379")
	s.Require().NoError(err)
	s.redisAddr = fmt.Sprintf("%s:%s", host, port.Port())

	s.rdb = redis.NewClient(&redis.Options{Addr: s.redisAddr})
	s.Require().NoError(s.rdb.Ping(s.ctx).Err(), "ping dragonfly")

	// Pulsar standalone container. ~20s startup cost amortized across the suite.
	pc, err := pulsarmodule.Run(s.ctx, "apachepulsar/pulsar:4.0.2")
	s.Require().NoError(err, "start pulsar container")
	s.pulsarContainer = pc
	brokerURL, err := pc.BrokerURL(s.ctx)
	s.Require().NoError(err)
	s.pulsarURL = brokerURL
}

func (s *DispatchSuite) TearDownSuite() {
	if s.rdb != nil {
		_ = s.rdb.Close()
	}
	if s.dragonflyContainer != nil {
		_ = s.dragonflyContainer.Terminate(s.ctx)
	}
	if s.pulsarContainer != nil {
		_ = s.pulsarContainer.Terminate(s.ctx)
	}
}

func (s *DispatchSuite) SetupTest() {
	// Clean slate per case so cross-test contamination can't hide bugs.
	s.Require().NoError(s.rdb.FlushDB(s.ctx).Err(), "flush dragonfly between tests")
}

// ============================================================================
// Test helpers
// ============================================================================

// newClientUnderTest wires a TranscribeClient against the real Dragonfly container and the
// supplied Slack/ML mocks. Pulsar is not wired here; tests that need it use newClientWithPulsar.
func (s *DispatchSuite) newClientUnderTest(slackMock SlackPoster, mlMock MLClient) *TranscribeClient {
	dfly, err := dragonfly.NewClient(s.ctx, 2*time.Second, &redis.Options{Addr: s.redisAddr})
	s.Require().NoError(err)

	cfg := &config.Config{
		SlackChannelID:                    "C-TEST",
		SlackTimeout:                      2 * time.Second,
		TacticalChannelActivationDuration: 30 * time.Minute,
		TACSweeperInterval:                100 * time.Millisecond,
		DedupTTL:                          1 * time.Hour,
		WorkerTimeout:                     5 * time.Second,
	}

	return newTranscribeClientForTest(cfg, nil, nil, nil, mlMock, slackMock, dfly)
}

func dispatchMessages(messages ...ml.DispatchMessage) *ml.DispatchMessages {
	return &ml.DispatchMessages{Messages: messages}
}

// ============================================================================
// processDispatchCall: 5 cases
// ============================================================================

func (s *DispatchSuite) TestProcessDispatchCall_HappyPath_TrailRescue() {
	slackMock := new(mockSlackPoster)
	mlMock := new(mockMLClient)
	tc := s.newClientUnderTest(slackMock, mlMock)

	mlMock.On("ParseRelevantInformationFromDispatchMessage", mock.Anything, "raw").Return(
		dispatchMessages(ml.DispatchMessage{
			CallType:             "Rescue - Trail",
			TACChannel:           "TAC1",
			CleanedTranscription: "rescue trail call",
		}), nil,
	)
	slackMock.On("SendMessageContext", mock.Anything, "C-TEST", mock.Anything).
		Return("C-TEST", "ts-rescue-1", "", nil).Once()

	parsed := &AdornedDeconstructedKey{dk: &DeconstructedKey{Talkgroup: FireDispatch1TGID}}
	tr := stubASRResponse("raw")

	err := tc.processDispatchCall(s.ctx, parsed, tr)
	s.Require().NoError(err)

	// Allowed_talkgroups should now contain TAC1's TGID.
	tg := talkgroupFromRadioShortCode["TAC1"]
	isMember, err := tc.dragonflyClient.SMisMember(s.ctx, "allowed_talkgroups", tg.TGID)
	s.Require().NoError(err)
	s.Equal([]bool{true}, isMember, "TAC1 should be allow-listed after dispatch")

	// Per-talkgroup thread key should hold the returned ts.
	thread, err := tc.dragonflyClient.Get(s.ctx, fmt.Sprintf(talkgroupKeyPrefix, tg.TGID))
	s.Require().NoError(err)
	s.Equal("ts-rescue-1", thread, "thread_ts must be persisted under tg:%s", tg.TGID)

	// Sweeper ZSET should hold one pending closure.
	members, err := s.rdb.ZRange(s.ctx, activeTACsKey, 0, -1).Result()
	s.Require().NoError(err)
	s.Len(members, 1, "exactly one pending TAC closure scheduled")

	slackMock.AssertExpectations(s.T())
	mlMock.AssertExpectations(s.T())
}

func (s *DispatchSuite) TestProcessDispatchCall_NoTrailRescue_IsNoOp() {
	slackMock := new(mockSlackPoster)
	mlMock := new(mockMLClient)
	tc := s.newClientUnderTest(slackMock, mlMock)

	mlMock.On("ParseRelevantInformationFromDispatchMessage", mock.Anything, "raw").Return(
		dispatchMessages(ml.DispatchMessage{
			CallType:             "Aid Emergency",
			TACChannel:           "TAC2",
			CleanedTranscription: "aid call",
		}), nil,
	)

	parsed := &AdornedDeconstructedKey{dk: &DeconstructedKey{Talkgroup: FireDispatch1TGID}}
	err := tc.processDispatchCall(s.ctx, parsed, stubASRResponse("raw"))
	s.Require().NoError(err)

	// No Slack post, no ZSET entry.
	slackMock.AssertNotCalled(s.T(), "SendMessageContext", mock.Anything, mock.Anything, mock.Anything)
	count, err := s.rdb.ZCard(s.ctx, activeTACsKey).Result()
	s.Require().NoError(err)
	s.EqualValues(0, count)
}

func (s *DispatchSuite) TestProcessDispatchCall_UnknownTACChannel_ReturnsError() {
	slackMock := new(mockSlackPoster)
	mlMock := new(mockMLClient)
	tc := s.newClientUnderTest(slackMock, mlMock)

	mlMock.On("ParseRelevantInformationFromDispatchMessage", mock.Anything, "raw").Return(
		dispatchMessages(ml.DispatchMessage{
			CallType:             "Rescue - Trail",
			TACChannel:           "TAC99", // not a real channel
			CleanedTranscription: "bad TAC",
		}), nil,
	)

	parsed := &AdornedDeconstructedKey{dk: &DeconstructedKey{Talkgroup: FireDispatch1TGID}}
	err := tc.processDispatchCall(s.ctx, parsed, stubASRResponse("raw"))
	s.Require().Error(err)
	s.ErrorIs(err, ErrFailedToFindTalkgroup)

	// Bail-out is before SAddEx and Slack post.
	count, err := s.rdb.SCard(s.ctx, "allowed_talkgroups").Result()
	s.Require().NoError(err)
	s.EqualValues(0, count, "allow-list must be untouched on unknown TAC")
	slackMock.AssertNotCalled(s.T(), "SendMessageContext", mock.Anything, mock.Anything, mock.Anything)
}

func (s *DispatchSuite) TestProcessDispatchCall_SlackInitialPostFails_BailsBeforeScheduling() {
	slackMock := new(mockSlackPoster)
	mlMock := new(mockMLClient)
	tc := s.newClientUnderTest(slackMock, mlMock)

	mlMock.On("ParseRelevantInformationFromDispatchMessage", mock.Anything, "raw").Return(
		dispatchMessages(ml.DispatchMessage{
			CallType:             "Rescue - Trail",
			TACChannel:           "TAC1",
			CleanedTranscription: "rescue",
		}), nil,
	)
	slackMock.On("SendMessageContext", mock.Anything, "C-TEST", mock.Anything).
		Return("", "", "", errors.New("slack down")).Once()

	parsed := &AdornedDeconstructedKey{dk: &DeconstructedKey{Talkgroup: FireDispatch1TGID}}
	err := tc.processDispatchCall(s.ctx, parsed, stubASRResponse("raw"))
	s.Require().Error(err)
	s.ErrorIs(err, ErrFailedToPostSlackMessage)

	// FIX (review item #1): the regression we're guarding against — a failed Slack post used to
	// fall through and schedule a TAC closure against an empty thread_ts. ZSET must stay empty.
	count, err := s.rdb.ZCard(s.ctx, activeTACsKey).Result()
	s.Require().NoError(err)
	s.EqualValues(0, count, "no closure scheduled when initial Slack post failed")
}

func (s *DispatchSuite) TestProcessDispatchCall_RateLimitedThenSucceeds() {
	slackMock := new(mockSlackPoster)
	mlMock := new(mockMLClient)
	tc := s.newClientUnderTest(slackMock, mlMock)

	mlMock.On("ParseRelevantInformationFromDispatchMessage", mock.Anything, "raw").Return(
		dispatchMessages(ml.DispatchMessage{
			CallType:             "Rescue - Trail",
			TACChannel:           "TAC1",
			CleanedTranscription: "rescue",
		}), nil,
	)

	// FIX (review item #1): the prior handleSlackRateLimit returned nil after waiting and never
	// re-sent. This case proves sendSlackWithRetry actually issues a second SendMessageContext.
	slackMock.On("SendMessageContext", mock.Anything, "C-TEST", mock.Anything).
		Return("", "", "", &slack.RateLimitedError{RetryAfter: 50 * time.Millisecond}).Once()
	slackMock.On("SendMessageContext", mock.Anything, "C-TEST", mock.Anything).
		Return("C-TEST", "ts-after-retry", "", nil).Once()

	parsed := &AdornedDeconstructedKey{dk: &DeconstructedKey{Talkgroup: FireDispatch1TGID}}
	err := tc.processDispatchCall(s.ctx, parsed, stubASRResponse("raw"))
	s.Require().NoError(err)

	tg := talkgroupFromRadioShortCode["TAC1"]
	thread, err := tc.dragonflyClient.Get(s.ctx, fmt.Sprintf(talkgroupKeyPrefix, tg.TGID))
	s.Require().NoError(err)
	s.Equal("ts-after-retry", thread, "second-attempt ts must be persisted")

	slackMock.AssertNumberOfCalls(s.T(), "SendMessageContext", 2)
}

// ============================================================================
// processNonDispatchCall: 3 cases
// ============================================================================

func (s *DispatchSuite) TestProcessNonDispatchCall_HappyPath_PostsInThread() {
	slackMock := new(mockSlackPoster)
	mlMock := new(mockMLClient)
	tc := s.newClientUnderTest(slackMock, mlMock)

	tac1TGID := talkgroupFromRadioShortCode["TAC1"].TGID
	s.Require().NoError(tc.dragonflyClient.Set(
		s.ctx, fmt.Sprintf(talkgroupKeyPrefix, tac1TGID), 30*time.Minute, "ts-parent",
	))

	slackMock.On("SendMessageContext", mock.Anything, "C-TEST", mock.Anything).
		Return("C-TEST", "ts-child", "", nil).Once()

	parsed := &AdornedDeconstructedKey{dk: &DeconstructedKey{Talkgroup: tac1TGID}}
	err := tc.processNonDispatchCall(s.ctx, parsed, stubASRResponse("update"))
	s.Require().NoError(err)
	slackMock.AssertExpectations(s.T())
}

func (s *DispatchSuite) TestProcessNonDispatchCall_ThreadIDMissing_ReturnsError() {
	slackMock := new(mockSlackPoster)
	mlMock := new(mockMLClient)
	tc := s.newClientUnderTest(slackMock, mlMock)

	parsed := &AdornedDeconstructedKey{dk: &DeconstructedKey{Talkgroup: "1389"}}
	err := tc.processNonDispatchCall(s.ctx, parsed, stubASRResponse("update"))
	s.Require().Error(err)
	s.ErrorIs(err, ErrFailedToGetThreadIDFromDragonfly)
	slackMock.AssertNotCalled(s.T(), "SendMessageContext", mock.Anything, mock.Anything, mock.Anything)
}

func (s *DispatchSuite) TestProcessNonDispatchCall_UnknownTalkgroup_ReturnsError() {
	slackMock := new(mockSlackPoster)
	mlMock := new(mockMLClient)
	tc := s.newClientUnderTest(slackMock, mlMock)

	// Pre-populate a thread for an unknown TGID so we get past the Get check and exercise the
	// talkgroupFromTGID lookup branch.
	s.Require().NoError(tc.dragonflyClient.Set(
		s.ctx, fmt.Sprintf(talkgroupKeyPrefix, "9999"), 30*time.Minute, "ts-orphan",
	))

	parsed := &AdornedDeconstructedKey{dk: &DeconstructedKey{Talkgroup: "9999"}}
	err := tc.processNonDispatchCall(s.ctx, parsed, stubASRResponse("update"))
	s.Require().Error(err)
	s.ErrorIs(err, ErrFailedToFindTalkgroup)
	slackMock.AssertNotCalled(s.T(), "SendMessageContext", mock.Anything, mock.Anything, mock.Anything)
}

// ============================================================================
// Sweep: 3 cases
// ============================================================================

// scheduleClosureFixture writes a TGID into the ZSET and the matching ClosureMeta into
// the sibling tac_meta:<TGID> key, mirroring what ScheduleTACClosure does in production.
func (s *DispatchSuite) scheduleClosureFixture(tgid string, score int64, meta ClosureMeta) {
	payload, err := json.Marshal(meta)
	s.Require().NoError(err)
	s.Require().NoError(s.rdb.Set(s.ctx, fmt.Sprintf(tacMetaKeyFmt, tgid), payload, 24*time.Hour).Err())
	s.Require().NoError(s.rdb.ZAdd(s.ctx, activeTACsKey, redis.Z{Score: float64(score), Member: tgid}).Err())
}

func (s *DispatchSuite) TestSweep_PicksUpDueClosure() {
	slackMock := new(mockSlackPoster)
	mlMock := new(mockMLClient)
	tc := s.newClientUnderTest(slackMock, mlMock)

	tgid := talkgroupFromRadioShortCode["TAC1"].TGID
	s.scheduleClosureFixture(tgid, time.Now().Add(-1*time.Second).Unix(), ClosureMeta{
		TGID: tgid, TACChannel: "TAC1", ThreadTS: "ts-1", SourceTalkgroup: FireDispatch1TGID, MessageTS: "ts-1",
		Transcription: "Rescue Trail TAC 1 ...",
	})

	// Alert rewrite (chat.update) must fire for closures with stored transcription. Mock
	// returns generic OK; the body asserts come from the SendMessageContext expectation
	// for the thread reply that follows.
	slackMock.On("UpdateMessageContext", mock.Anything, "C-TEST", "ts-1", mock.Anything).
		Return("C-TEST", "ts-1", "", nil).Once()
	slackMock.On("SendMessageContext", mock.Anything, "C-TEST", mock.Anything).
		Return("C-TEST", "ts-closed", "", nil).Once()

	tc.sweepOnce(s.ctx)

	slackMock.AssertExpectations(s.T())
	count, err := s.rdb.ZCard(s.ctx, activeTACsKey).Result()
	s.Require().NoError(err)
	s.EqualValues(0, count, "ZSET should be empty after the entry is consumed")

	// Metadata sibling must also be cleaned up so it doesn't linger to the safety-net TTL.
	exists, err := s.rdb.Exists(s.ctx, fmt.Sprintf(tacMetaKeyFmt, tgid)).Result()
	s.Require().NoError(err)
	s.EqualValues(0, exists, "tac_meta:<TGID> must be deleted alongside the ZSET entry")
}

// FIX (feedback URL prefill): the sweeper's sidecar cleanup must run AFTER postChannelClosed,
// not before — the feedback-URL builder reads summary_data:<TGID>, and an early Del would
// silently strip the headline + situation_summary prefill from the Google Form URL. This
// test catches the regression by asserting summary_data is still present at the moment
// UpdateMessageContext fires (which happens INSIDE postChannelClosed, after buildFeedbackURL
// has used it).
func (s *DispatchSuite) TestSweep_SummaryDataAliveDuringChatUpdate() {
	slackMock := new(mockSlackPoster)
	mlMock := new(mockMLClient)
	tc := s.newClientUnderTest(slackMock, mlMock)

	tgid := talkgroupFromRadioShortCode["TAC1"].TGID
	s.scheduleClosureFixture(tgid, time.Now().Add(-1*time.Second).Unix(), ClosureMeta{
		TGID: tgid, TACChannel: "TAC1", ThreadTS: "ts-1", SourceTalkgroup: FireDispatch1TGID, MessageTS: "ts-1",
		Transcription: "Original dispatch text.",
	})
	// Pre-load the summary that the feedback URL would prefill from.
	summaryJSON, _ := json.Marshal(ml.RescueSummary{
		Headline:         "Bicycle accident on Tiger Mountain Trail",
		SituationSummary: "Patient ambulatory; resources canceled.",
	})
	s.Require().NoError(s.rdb.Set(s.ctx, fmt.Sprintf(summaryDataKeyFmt, tgid), summaryJSON, 1*time.Hour).Err())

	// At the moment UpdateMessageContext fires, summary_data must still exist — this is the
	// invariant the regression busted (cleanup was running BEFORE the feedback URL was built).
	slackMock.On("UpdateMessageContext", mock.Anything, "C-TEST", "ts-1", mock.Anything).
		Return("C-TEST", "ts-1", "", nil).Run(func(args mock.Arguments) {
			exists, err := s.rdb.Exists(s.ctx, fmt.Sprintf(summaryDataKeyFmt, tgid)).Result()
			s.Require().NoError(err)
			s.EqualValues(1, exists, "summary_data must still exist when chat.update fires; otherwise the feedback URL builder couldn't have used the headline/situation summary")
		}).Once()
	slackMock.On("SendMessageContext", mock.Anything, "C-TEST", mock.Anything).
		Return("C-TEST", "ts-closed", "", nil).Once()

	tc.sweepOnce(s.ctx)

	slackMock.AssertExpectations(s.T())

	// Cleanup still runs — just AFTER the post. Asserting this confirms we didn't accidentally
	// stop deleting the sidecar.
	exists, err := s.rdb.Exists(s.ctx, fmt.Sprintf(summaryDataKeyFmt, tgid)).Result()
	s.Require().NoError(err)
	s.EqualValues(0, exists, "summary_data must be deleted after the post completes")
}

// Without a stored transcription (older meta in flight from before the field existed) the
// sweeper SHOULD NOT call UpdateMessageContext — losing context on the alert is worse than
// leaving the buttons stale. The thread reply still posts.
func (s *DispatchSuite) TestSweep_NoTranscription_SkipsAlertUpdateButPostsThread() {
	slackMock := new(mockSlackPoster)
	mlMock := new(mockMLClient)
	tc := s.newClientUnderTest(slackMock, mlMock)

	tgid := talkgroupFromRadioShortCode["TAC1"].TGID
	s.scheduleClosureFixture(tgid, time.Now().Add(-1*time.Second).Unix(), ClosureMeta{
		TGID: tgid, TACChannel: "TAC1", ThreadTS: "ts-1", SourceTalkgroup: FireDispatch1TGID, MessageTS: "ts-1",
		// Transcription deliberately empty.
	})

	slackMock.On("SendMessageContext", mock.Anything, "C-TEST", mock.Anything).
		Return("C-TEST", "ts-closed", "", nil).Once()

	tc.sweepOnce(s.ctx)

	slackMock.AssertNotCalled(s.T(), "UpdateMessageContext", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	slackMock.AssertExpectations(s.T())
}

func (s *DispatchSuite) TestSweep_DoesNotTouchFutureClosures() {
	slackMock := new(mockSlackPoster)
	mlMock := new(mockMLClient)
	tc := s.newClientUnderTest(slackMock, mlMock)

	tgid := talkgroupFromRadioShortCode["TAC1"].TGID
	s.scheduleClosureFixture(tgid, time.Now().Add(1*time.Hour).Unix(), ClosureMeta{
		TGID: tgid, TACChannel: "TAC1", ThreadTS: "ts-future", SourceTalkgroup: FireDispatch1TGID, MessageTS: "ts-future",
	})

	tc.sweepOnce(s.ctx)

	slackMock.AssertNotCalled(s.T(), "SendMessageContext", mock.Anything, mock.Anything, mock.Anything)
	count, err := s.rdb.ZCard(s.ctx, activeTACsKey).Result()
	s.Require().NoError(err)
	s.EqualValues(1, count, "future closure must remain in the ZSET")
}

func (s *DispatchSuite) TestSweep_OrphanedZSETEntryIsCleanedNoPost() {
	// Schedule an entry with NO sibling metadata key. Sweeper must claim it via ZRem (so it
	// doesn't loop forever) but must not attempt to post anything to Slack.
	slackMock := new(mockSlackPoster)
	mlMock := new(mockMLClient)
	tc := s.newClientUnderTest(slackMock, mlMock)

	pastSecondsAgo := time.Now().Add(-1 * time.Second).Unix()
	s.Require().NoError(s.rdb.ZAdd(s.ctx, activeTACsKey, redis.Z{
		Score: float64(pastSecondsAgo), Member: "9999",
	}).Err())

	tc.sweepOnce(s.ctx)

	slackMock.AssertNotCalled(s.T(), "SendMessageContext", mock.Anything, mock.Anything, mock.Anything)
	count, err := s.rdb.ZCard(s.ctx, activeTACsKey).Result()
	s.Require().NoError(err)
	s.EqualValues(0, count, "orphan entry must be removed even though metadata is missing")
}

// ============================================================================
// Live interpretation (rolling summary): exercises updateLiveInterpretation directly,
// asserting the per-call behavior (append, summarize, post-or-update) without going
// through the full Pulsar lifecycle.
// ============================================================================

func (s *DispatchSuite) TestLiveInterpretation_FirstTAC_PostsAndCachesSummaryTS() {
	slackMock := new(mockSlackPoster)
	mlMock := new(mockMLClient)
	tc := s.newClientUnderTest(slackMock, mlMock)

	// Pre-load the closure metadata so the helper has the dispatch context.
	tgid := talkgroupFromRadioShortCode["TAC10"].TGID
	meta := ClosureMeta{
		TGID: tgid, TACChannel: "TAC10", ThreadTS: "ts-rescue", SourceTalkgroup: FireDispatch1TGID,
		MessageTS: "ts-rescue", Transcription: "Rescue Trail TAC 10 ...",
	}
	payload, _ := json.Marshal(meta)
	s.Require().NoError(tc.dragonflyClient.Set(s.ctx, fmt.Sprintf(tacMetaKeyFmt, tgid), 1*time.Hour, string(payload)))

	mlMock.On("SummarizeRescue", mock.Anything, mock.MatchedBy(func(in ml.RescueSummaryInput) bool {
		// Confirm cumulative context is being passed: dispatch + the one TAC entry.
		return in.DispatchTranscription == meta.Transcription &&
			in.TACChannel == "TAC10" &&
			len(in.TACTranscripts) == 1 &&
			in.TACTranscripts[0].Text == "first transmission"
	})).Return(&ml.RescueSummary{Headline: "h1"}, nil).Once()

	// First TAC → no cached summary_ts → SendMessageContext fires (post a new threaded msg).
	slackMock.On("SendMessageContext", mock.Anything, "C-TEST", mock.Anything).
		Return("C-TEST", "ts-summary-1", "", nil).Once()

	tc.updateLiveInterpretation(s.ctx, tgid, time.Now(), "first transmission")

	mlMock.AssertExpectations(s.T())
	slackMock.AssertExpectations(s.T())

	// summary_ts must be cached so the next transmission updates instead of re-posting.
	cached, err := s.rdb.Get(s.ctx, fmt.Sprintf(summaryTSKeyFmt, tgid)).Result()
	s.Require().NoError(err)
	s.Equal("ts-summary-1", cached)

	// Transcripts list must contain exactly one entry.
	count, err := s.rdb.LLen(s.ctx, fmt.Sprintf(tacTranscriptsKeyFmt, tgid)).Result()
	s.Require().NoError(err)
	s.EqualValues(1, count)
}

func (s *DispatchSuite) TestLiveInterpretation_SubsequentTAC_ChatUpdatesExistingMessage() {
	slackMock := new(mockSlackPoster)
	mlMock := new(mockMLClient)
	tc := s.newClientUnderTest(slackMock, mlMock)

	tgid := talkgroupFromRadioShortCode["TAC10"].TGID
	meta := ClosureMeta{
		TGID: tgid, TACChannel: "TAC10", ThreadTS: "ts-rescue", SourceTalkgroup: FireDispatch1TGID,
		MessageTS: "ts-rescue", Transcription: "Rescue Trail TAC 10 ...",
	}
	payload, _ := json.Marshal(meta)
	s.Require().NoError(tc.dragonflyClient.Set(s.ctx, fmt.Sprintf(tacMetaKeyFmt, tgid), 1*time.Hour, string(payload)))

	// Simulate an earlier transmission having already populated state.
	prior, _ := json.Marshal(liveTranscriptEntry{CapturedAt: "11:14:33", Text: "earlier"})
	s.Require().NoError(tc.dragonflyClient.RPush(s.ctx, fmt.Sprintf(tacTranscriptsKeyFmt, tgid), string(prior)))
	s.Require().NoError(tc.dragonflyClient.Set(s.ctx, fmt.Sprintf(summaryTSKeyFmt, tgid), 1*time.Hour, "ts-summary-1"))

	mlMock.On("SummarizeRescue", mock.Anything, mock.MatchedBy(func(in ml.RescueSummaryInput) bool {
		// Cumulative: BOTH transcripts (the earlier one + the new one) reach the model.
		return len(in.TACTranscripts) == 2 &&
			in.TACTranscripts[0].Text == "earlier" &&
			in.TACTranscripts[1].Text == "follow-up"
	})).Return(&ml.RescueSummary{Headline: "h2"}, nil).Once()

	// Subsequent TAC → cached summary_ts → UpdateMessageContext fires, NOT SendMessageContext.
	slackMock.On("UpdateMessageContext", mock.Anything, "C-TEST", "ts-summary-1", mock.Anything).
		Return("C-TEST", "ts-summary-1", "", nil).Once()

	tc.updateLiveInterpretation(s.ctx, tgid, time.Now(), "follow-up")

	slackMock.AssertNotCalled(s.T(), "SendMessageContext", mock.Anything, mock.Anything, mock.Anything)
	mlMock.AssertExpectations(s.T())
	slackMock.AssertExpectations(s.T())
}

// FIX (concurrent burst): when N transmissions arrive simultaneously, only ONE summarize
// runs initially. The others should mark stale and bail out. The lock holder picks up the
// stale flag and runs exactly one catch-up pass that reflects every transcript pushed
// during the first call.
func (s *DispatchSuite) TestLiveInterpretation_ConcurrentBurst_BoundedToTwoLLMCalls() {
	slackMock := new(mockSlackPoster)
	mlMock := new(mockMLClient)
	tc := s.newClientUnderTest(slackMock, mlMock)

	tgid := talkgroupFromRadioShortCode["TAC10"].TGID
	meta := ClosureMeta{
		TGID: tgid, TACChannel: "TAC10", ThreadTS: "ts-rescue", SourceTalkgroup: FireDispatch1TGID,
		MessageTS: "ts-rescue", Transcription: "Rescue Trail TAC 10 ...",
	}
	payload, _ := json.Marshal(meta)
	s.Require().NoError(tc.dragonflyClient.Set(s.ctx, fmt.Sprintf(tacMetaKeyFmt, tgid), 1*time.Hour, string(payload)))

	// gate is closed at start; the first SummarizeRescue call blocks on it. While it's
	// blocked we fire 3 more updates from "other workers" — each should RPush, fail to
	// acquire the lock, and set the stale flag. When we release the gate, the first call
	// returns; the lock holder sees the stale flag and runs exactly one more pass that now
	// sees all 4 transcripts.
	gate := make(chan struct{})

	mlMock.On("SummarizeRescue", mock.Anything, mock.MatchedBy(func(in ml.RescueSummaryInput) bool {
		return len(in.TACTranscripts) == 1
	})).Return(&ml.RescueSummary{Headline: "h1"}, nil).Once().Run(func(args mock.Arguments) {
		<-gate // First call (1 transcript) blocks until we tell it to return.
	})
	mlMock.On("SummarizeRescue", mock.Anything, mock.MatchedBy(func(in ml.RescueSummaryInput) bool {
		return len(in.TACTranscripts) == 4
	})).Return(&ml.RescueSummary{Headline: "h2"}, nil).Once()

	// Slack: the first pass posts a new message, the catch-up pass updates that message.
	slackMock.On("SendMessageContext", mock.Anything, "C-TEST", mock.Anything).
		Return("C-TEST", "ts-summary-1", "", nil).Once()
	slackMock.On("UpdateMessageContext", mock.Anything, "C-TEST", "ts-summary-1", mock.Anything).
		Return("C-TEST", "ts-summary-1", "", nil).Once()

	// Worker A in a goroutine — will block inside SummarizeRescue until the gate releases.
	done := make(chan struct{})
	go func() {
		defer close(done)
		tc.updateLiveInterpretation(s.ctx, tgid, time.Now(), "transmission 1")
	}()

	// Wait for the lock to actually be held before firing the burst (otherwise we're racing
	// the goroutine's setup). Poll Dragonfly directly.
	s.Require().Eventually(func() bool {
		v, _ := s.rdb.Get(s.ctx, fmt.Sprintf(summaryLockKeyFmt, tgid)).Result()
		return v == "1"
	}, 2*time.Second, 20*time.Millisecond, "expected first worker to hold the summary lock")

	// Burst: 3 more transmissions. Each should RPush and bail out without an LLM call.
	tc.updateLiveInterpretation(s.ctx, tgid, time.Now(), "transmission 2")
	tc.updateLiveInterpretation(s.ctx, tgid, time.Now(), "transmission 3")
	tc.updateLiveInterpretation(s.ctx, tgid, time.Now(), "transmission 4")

	// Stale flag must now be set — proves losers correctly signaled the lock holder.
	stale, err := s.rdb.Get(s.ctx, fmt.Sprintf(summaryStaleKeyFmt, tgid)).Result()
	s.Require().NoError(err)
	s.Equal("1", stale, "concurrent transmissions must mark the rescue stale")

	// Release the gate so worker A's first SummarizeRescue returns. It should then loop
	// and run the catch-up pass with all 4 transcripts.
	close(gate)
	<-done

	// Exactly two LLM calls — the initial (1 transcript) + the catch-up (4 transcripts).
	// AssertExpectations also verifies SendMessage and UpdateMessage each fired once.
	mlMock.AssertExpectations(s.T())
	slackMock.AssertExpectations(s.T())

	// Stale flag must be cleared by the lock holder before exit.
	staleAfter, _ := s.rdb.Get(s.ctx, fmt.Sprintf(summaryStaleKeyFmt, tgid)).Result()
	s.Empty(staleAfter, "stale flag must be cleared after the catch-up pass")

	// Lock must be released.
	lockAfter, _ := s.rdb.Get(s.ctx, fmt.Sprintf(summaryLockKeyFmt, tgid)).Result()
	s.Empty(lockAfter, "summary lock must be released after the catch-up pass")
}

func (s *DispatchSuite) TestLiveInterpretation_NoMeta_IsNoOp() {
	slackMock := new(mockSlackPoster)
	mlMock := new(mockMLClient)
	tc := s.newClientUnderTest(slackMock, mlMock)

	// No tac_meta: rescue isn't active. We still RPush the transcript (cheap/harmless), but
	// must NOT call ML or Slack.
	tc.updateLiveInterpretation(s.ctx, "9999", time.Now(), "anything")

	mlMock.AssertNotCalled(s.T(), "SummarizeRescue", mock.Anything, mock.Anything)
	slackMock.AssertNotCalled(s.T(), "SendMessageContext", mock.Anything, mock.Anything, mock.Anything)
	slackMock.AssertNotCalled(s.T(), "UpdateMessageContext", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// processRecord-direct: no Pulsar harness needed because we're testing the function
// in isolation. The motivating bug is the production race where a TAC transmission arrives
// before its dispatch has finished ML-classifying.
func (s *DispatchSuite) TestProcessRecord_RejectedRecordDoesNotBurnDedup() {
	slackMock := new(mockSlackPoster)
	mlMock := new(mockMLClient)
	tc := s.newClientUnderTest(slackMock, mlMock)

	// TAC10 (1967) is NOT in allowed_talkgroups — IsObjectAllowed will reject.
	objectKey := "1967-1777832063_852162500.0-call_002.wav"
	record := &s3event.EventRecord{
		EventName: s3event.EventObjectCreatedPut,
		S3: s3event.EventS3Data{
			Object: s3event.EventObjectData{Key: objectKey},
		},
	}

	err := tc.processRecord(s.ctx, record)
	s.Require().NoError(err, "rejected records (no dispatch in flight) must return nil so handleMessage acks them")

	// FIX (race against allow-list timing): the dedup key MUST NOT be set for a rejected
	// record. Otherwise a redelivered or re-published copy of the same audio (e.g. arriving
	// after the dispatch puts the channel on the allow-list) would be silently skipped.
	exists, err := s.rdb.Exists(s.ctx, fmt.Sprintf(dedupKeyPrefix, objectKey)).Result()
	s.Require().NoError(err)
	s.EqualValues(0, exists, "rejected records must not burn a dedup slot")

	slackMock.AssertNotCalled(s.T(), "SendMessageContext", mock.Anything, mock.Anything, mock.Anything)
	mlMock.AssertNotCalled(s.T(), "ParseRelevantInformationFromDispatchMessage", mock.Anything, mock.Anything)
}

// FIX (dispatch-in-flight nack recovery): when a 1399 event is currently being processed,
// concurrent TAC events that hit the not-allowed branch must NACK so Pulsar redelivers.
// Otherwise they're ack-and-dropped before the dispatch's ML can populate the allow-list.
func (s *DispatchSuite) TestProcessRecord_TACWhileDispatchInFlight_ReturnsErrToNack() {
	slackMock := new(mockSlackPoster)
	mlMock := new(mockMLClient)
	tc := s.newClientUnderTest(slackMock, mlMock)

	// Simulate "dispatch worker is mid-ML": the marker is set but TAC10 isn't yet allowed.
	s.Require().NoError(tc.dragonflyClient.Set(s.ctx, dispatchInFlightKey, 90*time.Second, "1"))

	record := &s3event.EventRecord{
		EventName: s3event.EventObjectCreatedPut,
		S3: s3event.EventS3Data{
			Object: s3event.EventObjectData{Key: "1967-1777832063_852162500.0-call_002.wav"},
		},
	}

	err := tc.processRecord(s.ctx, record)
	s.Require().Error(err, "must return error so handleMessage nacks for Pulsar redelivery")
	s.Contains(err.Error(), "in-flight dispatch")

	// Dedup must still be untouched — this message hasn't been processed yet.
	exists, err := s.rdb.Exists(s.ctx, fmt.Sprintf(dedupKeyPrefix, "1967-1777832063_852162500.0-call_002.wav")).Result()
	s.Require().NoError(err)
	s.EqualValues(0, exists, "rejected-during-in-flight messages must not burn a dedup slot either")

	slackMock.AssertNotCalled(s.T(), "SendMessageContext", mock.Anything, mock.Anything, mock.Anything)
}

// FIX (dispatch-in-flight + dedup interaction): a stale dedup key (typical when re-running
// the synthetic trigger without a Dragonfly flush) caused the marker to be set with no
// actual dispatch processing behind it — every subsequent TAC transmission for the next
// WorkerTimeout window would nack-for-retry chasing an allow-list write that was never
// going to happen. The fix moved the marker set to AFTER the dedup check; this test pins it.
func (s *DispatchSuite) TestProcessRecord_DispatchDedupHit_DoesNotSetInFlightMarker() {
	slackMock := new(mockSlackPoster)
	mlMock := new(mockMLClient)
	tc := s.newClientUnderTest(slackMock, mlMock)

	// Simulate the prior-run-still-cached state: dedup key for the 1399 dispatch is set.
	dispatchKey := "1399-1777832036_852162500.0-call_001.wav"
	s.Require().NoError(s.rdb.Set(s.ctx, fmt.Sprintf(dedupKeyPrefix, dispatchKey), "1", 1*time.Hour).Err())

	record := &s3event.EventRecord{
		EventName: s3event.EventObjectCreatedPut,
		S3:        s3event.EventS3Data{Object: s3event.EventObjectData{Key: dispatchKey}},
	}

	err := tc.processRecord(s.ctx, record)
	s.Require().NoError(err, "dedup-hit must return nil so the message is acked")

	// The critical invariant: a dedup-hit dispatch must NOT leave dispatch_in_flight set.
	// If it did, every TAC transmission for the next WorkerTimeout window would nack-for-retry
	// chasing a dispatch that never actually ran.
	exists, err := s.rdb.Exists(s.ctx, dispatchInFlightKey).Result()
	s.Require().NoError(err)
	s.EqualValues(0, exists, "dispatch_in_flight must NOT be set when the dispatch was dedup-skipped")

	mlMock.AssertNotCalled(s.T(), "ParseRelevantInformationFromDispatchMessage", mock.Anything, mock.Anything)
	slackMock.AssertNotCalled(s.T(), "SendMessageContext", mock.Anything, mock.Anything, mock.Anything)
}

// FIX (dispatch_in_flight cleanup): processRecord defers a clear of the marker so it
// is always cleaned up after the dispatch path completes, regardless of outcome. This
// pins the regression where a non-rescue dispatch (e.g. the LLM classifies as
// "Smoke - Burn Complaint") left the marker set for the full WorkerTimeout window,
// causing every concurrent TAC transmission to nack-loop chasing an allow-list write
// that was never going to happen.
//
// The test exercises the panic-during-S3-fetch path (nil s3Client). Defers fire during
// panic unwind, so the marker should be cleared even though processRecord didn't return
// cleanly — that's the same property that protects us against unexpected panics in
// production code paths.
func (s *DispatchSuite) TestProcessRecord_DispatchPath_ClearsInFlightOnExit() {
	slackMock := new(mockSlackPoster)
	mlMock := new(mockMLClient)
	tc := s.newClientUnderTest(slackMock, mlMock)

	record := &s3event.EventRecord{
		EventName: s3event.EventObjectCreatedPut,
		S3: s3event.EventS3Data{
			Object: s3event.EventObjectData{Key: "1399-1777832036_852162500.0-call_001.wav"},
		},
	}

	// processRecord sets the marker, then panics at the nil s3Client. The deferred clear
	// in processRecord runs during panic unwind; we recover here to assert post-state.
	func() {
		defer func() { _ = recover() }()
		_ = tc.processRecord(s.ctx, record)
	}()

	exists, err := s.rdb.Exists(s.ctx, dispatchInFlightKey).Result()
	s.Require().NoError(err)
	s.EqualValues(0, exists, "dispatch_in_flight must be cleared by processRecord's defer, even on panic-unwind")
}


// ============================================================================
// handleMessage: 3 cases (uses the Pulsar container for real ack/nack lifecycle)
// ============================================================================

// pulsarHarness wires a Pulsar producer + consumer pair onto a fresh per-test topic so cases
// don't bleed into each other through subscription state.
type pulsarHarness struct {
	client       pulsarapi.Client
	producer     pulsarapi.Producer
	consumer     *internalpulsar.PulsarClient
	topic        string
	subscription string
}

func (s *DispatchSuite) newPulsarHarness(testName string) *pulsarHarness {
	topic := fmt.Sprintf("persistent://public/default/handlemessage-%s-%d", testName, time.Now().UnixNano())
	subscription := "test-sub"

	client, err := pulsarapi.NewClient(pulsarapi.ClientOptions{URL: s.pulsarURL})
	s.Require().NoError(err)

	producer, err := client.CreateProducer(pulsarapi.ProducerOptions{Topic: topic})
	s.Require().NoError(err)

	consumer, err := internalpulsar.NewPulsarClient(internalpulsar.Options{
		URL:                 s.pulsarURL,
		InputTopic:          topic,
		Subscription:        subscription,
		NackRedeliveryDelay: 1 * time.Second,
	})
	s.Require().NoError(err)

	return &pulsarHarness{
		client:       client,
		producer:     producer,
		consumer:     consumer,
		topic:        topic,
		subscription: subscription,
	}
}

func (h *pulsarHarness) close() {
	h.producer.Close()
	h.consumer.Close()
	h.client.Close()
}

// fireDispatchEvent builds a minimal S3 event payload for a fire-dispatch wav.
func fireDispatchEvent(key string) []byte {
	evt := s3event.EventSchema{
		Records: []s3event.EventRecord{{
			EventName: s3event.EventObjectCreatedPut,
			S3: s3event.EventS3Data{
				Object: s3event.EventObjectData{Key: key},
			},
		}},
	}
	b, _ := json.Marshal(evt)
	return b
}

func (s *DispatchSuite) TestHandleMessage_DedupHit_ShortCircuits() {
	slackMock := new(mockSlackPoster)
	mlMock := new(mockMLClient)
	tc := s.newClientUnderTest(slackMock, mlMock)

	h := s.newPulsarHarness("dedup")
	defer h.close()
	tc.pulsarClient = h.consumer

	// Pre-populate the dedup key for this object so processRecord short-circuits.
	objectKey := "1399-1750542445_854412500.1-call_1.wav"
	dedupKey := fmt.Sprintf(dedupKeyPrefix, objectKey)
	s.Require().NoError(s.rdb.Set(s.ctx, dedupKey, "1", 1*time.Hour).Err())

	_, err := h.producer.Send(s.ctx, &pulsarapi.ProducerMessage{Payload: fireDispatchEvent(objectKey)})
	s.Require().NoError(err)

	msg, err := h.consumer.Receive(s.ctx)
	s.Require().NoError(err)
	tc.handleMessage(s.ctx, msg)

	// FIX (review item #11): dedup hit means no S3 fetch, no ASR, no ML, no Slack.
	slackMock.AssertNotCalled(s.T(), "SendMessageContext", mock.Anything, mock.Anything, mock.Anything)
	mlMock.AssertNotCalled(s.T(), "ParseRelevantInformationFromDispatchMessage", mock.Anything, mock.Anything)
}

func (s *DispatchSuite) TestHandleMessage_NonWavEvent_AcksWithoutSideEffects() {
	slackMock := new(mockSlackPoster)
	mlMock := new(mockMLClient)
	tc := s.newClientUnderTest(slackMock, mlMock)

	h := s.newPulsarHarness("nonwav")
	defer h.close()
	tc.pulsarClient = h.consumer

	_, err := h.producer.Send(s.ctx, &pulsarapi.ProducerMessage{
		Payload: fireDispatchEvent("notes.txt"),
	})
	s.Require().NoError(err)

	msg, err := h.consumer.Receive(s.ctx)
	s.Require().NoError(err)
	tc.handleMessage(s.ctx, msg)

	slackMock.AssertNotCalled(s.T(), "SendMessageContext", mock.Anything, mock.Anything, mock.Anything)
	mlMock.AssertNotCalled(s.T(), "ParseRelevantInformationFromDispatchMessage", mock.Anything, mock.Anything)
}

func (s *DispatchSuite) TestHandleMessage_MalformedPayload_NacksWithoutSideEffects() {
	slackMock := new(mockSlackPoster)
	mlMock := new(mockMLClient)
	tc := s.newClientUnderTest(slackMock, mlMock)

	h := s.newPulsarHarness("malformed")
	defer h.close()
	tc.pulsarClient = h.consumer

	_, err := h.producer.Send(s.ctx, &pulsarapi.ProducerMessage{Payload: []byte("not-json")})
	s.Require().NoError(err)

	msg, err := h.consumer.Receive(s.ctx)
	s.Require().NoError(err)
	tc.handleMessage(s.ctx, msg)

	// FIX (review item #2): malformed payloads Nack (rather than the prior Ack-then-skip), but
	// the side effects of processRecord must not run. We assert the latter directly; redelivery
	// from the Nack is Pulsar's contract, not ours to re-test here.
	slackMock.AssertNotCalled(s.T(), "SendMessageContext", mock.Anything, mock.Anything, mock.Anything)
	mlMock.AssertNotCalled(s.T(), "ParseRelevantInformationFromDispatchMessage", mock.Anything, mock.Anything)
}

// ============================================================================
// Misc helpers
// ============================================================================

// stubASRResponse builds the small ASR payload our process methods consume; the test path
// never re-reads the filename, so we just thread the transcription text through.
func stubASRResponse(transcription string) *asr.TranscriptionResponse {
	return &asr.TranscriptionResponse{Transcription: transcription}
}
