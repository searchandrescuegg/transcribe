package transcribe

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/searchandrescuegg/transcribe/internal/ml"
	"github.com/slack-go/slack"
	"github.com/stretchr/testify/mock"
)

func (s *DispatchSuite) setBrief(tgid, loc string) {
	b, _ := json.Marshal(s.briefSummary(loc, false))
	s.Require().NoError(s.rdb.Set(s.ctx, fmt.Sprintf(summaryDataKeyFmt, tgid), string(b), time.Hour).Err())
}

// Final-review #2a: after Close the active_tacs score is now-1 until the sweeper's next tick; a
// refresh in that window must not render a live alert with a past expiry and live buttons.
func (s *DispatchSuite) TestRerenderParentAlert_SkipsWhenClosing() {
	slackMock := new(mockSlackPoster)
	tc := s.newClientUnderTest(slackMock, new(mockMLClient))
	tgid, meta := s.seedActiveRescue(tc)
	s.setBrief(tgid, "Trail A")
	s.Require().NoError(s.rdb.ZAdd(s.ctx, activeTACsKey, redisZ(time.Now().Add(-time.Second).Unix(), tgid)).Err())

	s.False(tc.rerenderParentAlert(s.ctx, meta, "fb"))
	slackMock.AssertNotCalled(s.T(), "UpdateMessageContext", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// Final-review #2b: a rate-limited refresh must re-read state before retrying, so a rescue that
// closed during the RetryAfter wait is not overwritten with a stale live render.
func (s *DispatchSuite) TestRerenderParentAlert_RateLimitRetrySkipsIfClosedDuringWait() {
	slackMock := new(mockSlackPoster)
	tc := s.newClientUnderTest(slackMock, new(mockMLClient))
	tgid, meta := s.seedActiveRescue(tc)
	s.setBrief(tgid, "Trail A")
	slackMock.On("UpdateMessageContext", mock.Anything, "C-TEST", meta.MessageTS, mock.Anything).Run(func(mock.Arguments) {
		s.Require().NoError(s.rdb.ZRem(s.ctx, activeTACsKey, tgid).Err())
	}).Return("", "", "", &slack.RateLimitedError{RetryAfter: 10 * time.Millisecond}).Once()
	var parent []string
	s.captureParentAlert(slackMock, meta.MessageTS, &parent)

	s.False(tc.rerenderParentAlert(s.ctx, meta, "fb"))
	s.Empty(parent, "no second parent update after the rescue closed during the wait")
	slackMock.AssertNumberOfCalls(s.T(), "UpdateMessageContext", 1)
}

// Final-review #2b: the retry rebuilds from current state rather than resending stale blocks.
func (s *DispatchSuite) TestRerenderParentAlert_RateLimitRetryRebuildsWithNewBrief() {
	slackMock := new(mockSlackPoster)
	tc := s.newClientUnderTest(slackMock, new(mockMLClient))
	tgid, meta := s.seedActiveRescue(tc)
	s.setBrief(tgid, "Trail A")
	slackMock.On("UpdateMessageContext", mock.Anything, "C-TEST", meta.MessageTS, mock.Anything).Run(func(mock.Arguments) {
		s.setBrief(tgid, "Trail B")
	}).Return("", "", "", &slack.RateLimitedError{RetryAfter: 10 * time.Millisecond}).Once()
	var parent []string
	s.captureParentAlert(slackMock, meta.MessageTS, &parent)

	s.True(tc.rerenderParentAlert(s.ctx, meta, "fb"))
	s.Require().Len(parent, 1)
	s.Contains(parent[0], "Trail B · 54F · ankle injury")
	s.NotContains(parent[0], "Trail A")
}

// Final-review #4: case/whitespace drift in the brief must not churn the parent alert.
func (s *DispatchSuite) TestBrief_TrivialDriftDoesNotRefreshParent() {
	slackMock, mlMock := new(mockSlackPoster), new(mockMLClient)
	tc := s.newClientUnderTest(slackMock, mlMock)
	tgid, meta := s.seedActiveRescue(tc)
	s.Require().NoError(s.rdb.Set(s.ctx, fmt.Sprintf(summaryTSKeyFmt, tgid), "ts-sum", time.Hour).Err())
	slackMock.On("UpdateMessageContext", mock.Anything, "C-TEST", "ts-sum", mock.Anything).Return("", "", "", nil)
	var parent []string
	s.captureParentAlert(slackMock, meta.MessageTS, &parent)

	mlMock.On("SummarizeRescue", mock.Anything, mock.Anything).Return(&ml.RescueSummary{Headline: "h", BriefSubject: "54F", BriefCondition: "Ankle injury"}, nil).Once()
	s.True(tc.runOneSummaryPass(s.ctx, tgid, false))
	s.Require().Len(parent, 1)
	mlMock.On("SummarizeRescue", mock.Anything, mock.Anything).Return(&ml.RescueSummary{Headline: "h", BriefSubject: "54f", BriefCondition: "ankle  injury"}, nil).Once()
	s.True(tc.runOneSummaryPass(s.ctx, tgid, false))
	s.Len(parent, 1, "case/whitespace-only brief drift must not re-render the parent")
}

// captureParentAlert records the rendered blocks of every chat.update to the parent alert.
func (s *DispatchSuite) captureParentAlert(slackMock *mockSlackPoster, messageTS string, into *[]string) {
	slackMock.On("UpdateMessageContext", mock.Anything, "C-TEST", messageTS, mock.Anything).Run(func(args mock.Arguments) {
		_, vals, err := slack.UnsafeApplyMsgOptions("", "C-TEST", "", args.Get(3).([]slack.MsgOption)...)
		s.Require().NoError(err)
		*into = append(*into, vals.Get("blocks"))
	}).Return("", "", "", nil)
}

func (s *DispatchSuite) briefSummary(loc string, sar bool) *ml.RescueSummary {
	return &ml.RescueSummary{Headline: "h", BriefLocation: loc, BriefSubject: "54F", BriefCondition: "ankle injury", SARNotified: sar}
}

func (s *DispatchSuite) TestBrief_ParentUpdatedOnlyWhenBriefChanges() {
	slackMock, mlMock := new(mockSlackPoster), new(mockMLClient)
	tc := s.newClientUnderTest(slackMock, mlMock)
	tgid, meta := s.seedActiveRescue(tc)
	s.Require().NoError(s.rdb.Set(s.ctx, fmt.Sprintf(summaryTSKeyFmt, tgid), "ts-sum", time.Hour).Err())
	slackMock.On("UpdateMessageContext", mock.Anything, "C-TEST", "ts-sum", mock.Anything).Return("", "", "", nil)
	var parent []string
	s.captureParentAlert(slackMock, meta.MessageTS, &parent)

	mlMock.On("SummarizeRescue", mock.Anything, mock.Anything).Return(s.briefSummary("Mailbox Peak", false), nil).Twice()
	s.True(tc.runOneSummaryPass(s.ctx, tgid, false))
	s.True(tc.runOneSummaryPass(s.ctx, tgid, false))
	s.Require().Len(parent, 1, "first brief renders once; identical brief does not re-render")
	s.Contains(parent[0], "Mailbox Peak · 54F · ankle injury")

	mlMock.On("SummarizeRescue", mock.Anything, mock.Anything).Return(s.briefSummary("Mount Si", false), nil).Once()
	s.True(tc.runOneSummaryPass(s.ctx, tgid, false))
	s.Require().Len(parent, 2)
	s.Contains(parent[1], "Mount Si · 54F · ankle injury")
}

func (s *DispatchSuite) TestBrief_SARFlipAndBriefChange_OneUpdateWithBoth() {
	slackMock, mlMock := new(mockSlackPoster), new(mockMLClient)
	tc := s.newClientUnderTest(slackMock, mlMock)
	tgid, meta := s.seedActiveRescue(tc)
	s.Require().NoError(s.rdb.Set(s.ctx, fmt.Sprintf(summaryTSKeyFmt, tgid), "ts-sum", time.Hour).Err())
	slackMock.On("UpdateMessageContext", mock.Anything, "C-TEST", "ts-sum", mock.Anything).Return("", "", "", nil)
	var parent []string
	s.captureParentAlert(slackMock, meta.MessageTS, &parent)

	mlMock.On("SummarizeRescue", mock.Anything, mock.Anything).Return(s.briefSummary("Mailbox Peak", true), nil).Once()
	s.True(tc.runOneSummaryPass(s.ctx, tgid, false))
	s.Require().Len(parent, 1)
	s.Contains(parent[0], "Mailbox Peak")
	s.Contains(parent[0], "Rescue notified")
}

// Review Focus #1: a dispatch correction's re-render must keep an existing brief.
func (s *DispatchSuite) TestBrief_DispatchCorrectionKeepsBrief() {
	slackMock := new(mockSlackPoster)
	tc := s.newClientUnderTest(slackMock, new(mockMLClient))
	tgid, meta := s.seedActiveRescue(tc)
	prev, _ := json.Marshal(s.briefSummary("Mailbox Peak", false))
	s.Require().NoError(s.rdb.Set(s.ctx, fmt.Sprintf(summaryDataKeyFmt, tgid), string(prev), time.Hour).Err())
	var parent []string
	s.captureParentAlert(slackMock, meta.MessageTS, &parent)

	target, err := tc.ResolveCorrectionTarget(s.ctx, meta.MessageTS, meta.ThreadTS)
	s.Require().NoError(err)
	s.Require().NoError(tc.ApplyTranscriptCorrection(s.ctx, target, "U1", "Rescue Trail TAC 10 Mailbox Peak", time.Now()))
	s.Require().Len(parent, 1)
	s.Contains(parent[0], "Mailbox Peak · 54F · ankle injury")
	s.Contains(parent[0], "Corrected by")
}

// Review Focus #5: the closed alert keeps the brief.
func (s *DispatchSuite) TestBrief_ClosedAlertKeepsBrief() {
	slackMock := new(mockSlackPoster)
	tc := s.newClientUnderTest(slackMock, new(mockMLClient))
	tgid := talkgroupFromRadioShortCode["TAC10"].TGID
	meta := ClosureMeta{TGID: tgid, TACChannel: "TAC10", ThreadTS: "ts-rescue", SourceTalkgroup: FireDispatch1TGID,
		MessageTS: "ts-rescue", Transcription: "Rescue Trail TAC 10"}
	s.scheduleClosureFixture(tgid, time.Now().Add(-time.Second).Unix(), meta)
	prev, _ := json.Marshal(s.briefSummary("Mailbox Peak", false))
	s.Require().NoError(s.rdb.Set(s.ctx, fmt.Sprintf(summaryDataKeyFmt, tgid), string(prev), time.Hour).Err())
	var parent []string
	s.captureParentAlert(slackMock, "ts-rescue", &parent)
	slackMock.On("SendMessageContext", mock.Anything, "C-TEST", mock.Anything).Return("", "ts-closed", "", nil).Once()

	tc.sweepOnce(s.ctx)
	s.Require().Len(parent, 1)
	s.Contains(parent[0], "Mailbox Peak · 54F · ankle injury")
}

// Final-review #3: the parent refresh runs after the Live Interpretation update, so a
// rate-limited parent update never delays the thread message.
func (s *DispatchSuite) TestBrief_ParentRefreshRunsAfterLiveInterpretation() {
	slackMock, mlMock := new(mockSlackPoster), new(mockMLClient)
	tc := s.newClientUnderTest(slackMock, mlMock)
	tgid, meta := s.seedActiveRescue(tc)
	s.Require().NoError(s.rdb.Set(s.ctx, fmt.Sprintf(summaryTSKeyFmt, tgid), "ts-sum", time.Hour).Err())
	var order []string
	for _, ts := range []string{"ts-sum", meta.MessageTS} {
		ts := ts
		slackMock.On("UpdateMessageContext", mock.Anything, "C-TEST", ts, mock.Anything).Run(func(mock.Arguments) {
			order = append(order, ts)
		}).Return("", "", "", nil)
	}
	mlMock.On("SummarizeRescue", mock.Anything, mock.Anything).Return(s.briefSummary("Trail A", true), nil).Once()
	s.True(tc.runOneSummaryPass(s.ctx, tgid, false))
	s.Equal([]string{"ts-sum", meta.MessageTS}, order)
}
