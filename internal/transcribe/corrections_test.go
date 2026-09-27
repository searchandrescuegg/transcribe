package transcribe

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/searchandrescuegg/transcribe/internal/ml"
	"github.com/stretchr/testify/mock"
)

// Legacy entries (pre-feature JSON) must parse as radio and keep list indexes aligned with
// the Dragonfly list so LSET hits the right element; unparseable entries keep their slot.
func (s *DispatchSuite) TestTranscriptEntries_LegacyAndNewMixed_IndexAligned() {
	tc := s.newClientUnderTest(new(mockSlackPoster), new(mockMLClient))
	tgid := talkgroupFromRadioShortCode["TAC10"].TGID
	key := fmt.Sprintf(tacTranscriptsKeyFmt, tgid)

	s.Require().NoError(s.rdb.RPush(s.ctx, key, `{"captured_at":"11:00:00","text":"legacy"}`).Err())
	s.Require().NoError(s.rdb.RPush(s.ctx, key, `not json`).Err())
	s.Require().NoError(tc.appendEntry(s.ctx, tgid, liveTranscriptEntry{
		CapturedAt: "11:01:00", Text: "new", Kind: entryKindRadio, SlackTS: "ts-3",
	}))

	entries, err := tc.readEntries(s.ctx, tgid)
	s.Require().NoError(err)
	s.Require().Len(entries, 3)
	s.Equal(entryKindRadio, entries[0].kind(), "missing kind defaults to radio")
	s.Equal(entryKindInvalid, entries[1].kind(), "garbage keeps its slot")

	idx, e, found, err := tc.findEntryBySlackTS(s.ctx, tgid, "ts-3", entryKindRadio)
	s.Require().NoError(err)
	s.True(found)
	s.EqualValues(2, idx)

	e.Corrected = "fixed"
	e.Correction = &TranscriptCorrection{By: "U1", At: time.Unix(0, 0).UTC(), Original: "new"}
	s.Require().NoError(tc.setEntry(s.ctx, tgid, idx, e))

	raw, err := s.rdb.LIndex(s.ctx, key, 2).Result()
	s.Require().NoError(err)
	var got liveTranscriptEntry
	s.Require().NoError(json.Unmarshal([]byte(raw), &got))
	s.Equal("fixed", got.effectiveText())

	ttl, err := s.rdb.TTL(s.ctx, key).Result()
	s.Require().NoError(err)
	s.Greater(ttl, time.Duration(0), "appendEntry must stamp a TTL")
}

func (s *DispatchSuite) TestDragonfly_GetDel() {
	tc := s.newClientUnderTest(new(mockSlackPoster), new(mockMLClient))
	s.Require().NoError(s.rdb.Set(s.ctx, "k", "v", time.Minute).Err())
	v, err := tc.dragonflyClient.GetDel(s.ctx, "k")
	s.Require().NoError(err)
	s.Equal("v", v)
	v, err = tc.dragonflyClient.GetDel(s.ctx, "k")
	s.Require().NoError(err)
	s.Equal("", v, "missing key returns empty string, not an error")
}

func (s *DispatchSuite) seedMeta(tc *TranscribeClient, tgid string) ClosureMeta {
	meta := ClosureMeta{TGID: tgid, TACChannel: "TAC10", ThreadTS: "ts-rescue", SourceTalkgroup: FireDispatch1TGID,
		MessageTS: "ts-rescue", Transcription: "Rescue Trail TAC 10 ..."}
	payload, _ := json.Marshal(meta)
	s.Require().NoError(tc.dragonflyClient.Set(s.ctx, fmt.Sprintf(tacMetaKeyFmt, tgid), time.Hour, string(payload)))
	return meta
}

// rewrite=true drops the previous summary so facts derived from pre-correction text can't linger.
func (s *DispatchSuite) TestRefresh_RewriteDropsPreviousSummary() {
	slackMock, mlMock := new(mockSlackPoster), new(mockMLClient)
	tc := s.newClientUnderTest(slackMock, mlMock)
	tgid := talkgroupFromRadioShortCode["TAC10"].TGID
	s.seedMeta(tc, tgid)
	s.Require().NoError(tc.appendEntry(s.ctx, tgid, liveTranscriptEntry{CapturedAt: "11:00:00", Text: "x", Kind: entryKindRadio}))
	prev, _ := json.Marshal(ml.RescueSummary{Headline: "old"})
	s.Require().NoError(s.rdb.Set(s.ctx, fmt.Sprintf(summaryDataKeyFmt, tgid), string(prev), time.Hour).Err())
	s.Require().NoError(s.rdb.Set(s.ctx, fmt.Sprintf(summaryTSKeyFmt, tgid), "ts-sum", time.Hour).Err())

	mlMock.On("SummarizeRescue", mock.Anything, mock.MatchedBy(func(in ml.RescueSummaryInput) bool {
		return in.PreviousSummary == nil
	})).Return(&ml.RescueSummary{Headline: "new"}, nil).Once()
	slackMock.On("UpdateMessageContext", mock.Anything, "C-TEST", "ts-sum", mock.Anything).Return("", "", "", nil).Once()

	tc.RefreshLiveInterpretation(s.ctx, tgid, true)
	mlMock.AssertExpectations(s.T())
}

// A lock loser asking for a rewrite must not be downgraded by a later plain transmission.
func (s *DispatchSuite) TestRefresh_PendingRewriteNeverDowngraded() {
	tc := s.newClientUnderTest(new(mockSlackPoster), new(mockMLClient))
	tgid := talkgroupFromRadioShortCode["TAC10"].TGID
	s.Require().NoError(s.rdb.Set(s.ctx, fmt.Sprintf(summaryLockKeyFmt, tgid), "1", time.Minute).Err()) // someone holds the lock

	tc.RefreshLiveInterpretation(s.ctx, tgid, true)  // loser → "rewrite"
	tc.RefreshLiveInterpretation(s.ctx, tgid, false) // loser → must NOT overwrite

	v, err := s.rdb.Get(s.ctx, fmt.Sprintf(summaryStaleKeyFmt, tgid)).Result()
	s.Require().NoError(err)
	s.Equal(staleValueRewrite, v)
}

// The lock holder consumes a pending "rewrite" (set while another pass ran) on its next pass.
func (s *DispatchSuite) TestRefresh_HolderHonorsPendingRewrite() {
	slackMock, mlMock := new(mockSlackPoster), new(mockMLClient)
	tc := s.newClientUnderTest(slackMock, mlMock)
	tgid := talkgroupFromRadioShortCode["TAC10"].TGID
	s.seedMeta(tc, tgid)
	s.Require().NoError(tc.appendEntry(s.ctx, tgid, liveTranscriptEntry{CapturedAt: "11:00:00", Text: "x", Kind: entryKindRadio}))
	s.Require().NoError(s.rdb.Set(s.ctx, fmt.Sprintf(summaryTSKeyFmt, tgid), "ts-sum", time.Hour).Err())
	slackMock.On("UpdateMessageContext", mock.Anything, "C-TEST", "ts-sum", mock.Anything).Return("", "", "", nil)

	// First pass (additive) sets a pending rewrite mid-flight, as a concurrent correction would.
	mlMock.On("SummarizeRescue", mock.Anything, mock.MatchedBy(func(in ml.RescueSummaryInput) bool {
		return in.PreviousSummary != nil
	})).Run(func(mock.Arguments) {
		_ = s.rdb.Set(s.ctx, fmt.Sprintf(summaryStaleKeyFmt, tgid), staleValueRewrite, time.Minute).Err()
	}).Return(&ml.RescueSummary{Headline: "a"}, nil).Once()
	mlMock.On("SummarizeRescue", mock.Anything, mock.MatchedBy(func(in ml.RescueSummaryInput) bool {
		return in.PreviousSummary == nil
	})).Return(&ml.RescueSummary{Headline: "b"}, nil).Once()

	prev, _ := json.Marshal(ml.RescueSummary{Headline: "old"})
	s.Require().NoError(s.rdb.Set(s.ctx, fmt.Sprintf(summaryDataKeyFmt, tgid), string(prev), time.Hour).Err())

	tc.RefreshLiveInterpretation(s.ctx, tgid, false)
	mlMock.AssertExpectations(s.T())
}

func (s *DispatchSuite) TestProcessNonDispatchCall_RecordsPostTSAndS3Key() {
	slackMock, mlMock := new(mockSlackPoster), new(mockMLClient)
	tc := s.newClientUnderTest(slackMock, mlMock)
	tgid := talkgroupFromRadioShortCode["TAC10"].TGID
	s.Require().NoError(tc.dragonflyClient.Set(s.ctx, fmt.Sprintf(talkgroupKeyPrefix, tgid), time.Hour, "ts-rescue"))
	// No tac_meta → the summary pass no-ops; we only care about the stored entry.
	slackMock.On("SendMessageContext", mock.Anything, "C-TEST", mock.Anything).Return("C-TEST", "ts-post-1", "", nil).Once()

	parsed := &AdornedDeconstructedKey{dk: &DeconstructedKey{Talkgroup: tgid, Time: time.Now()}, key: "2026/09/27/14/1967/obj.wav"}
	s.Require().NoError(tc.processNonDispatchCall(s.ctx, parsed, stubASRResponse("on scene")))

	idx, e, found, err := tc.findEntryBySlackTS(s.ctx, tgid, "ts-post-1", entryKindRadio)
	s.Require().NoError(err)
	s.Require().True(found)
	s.EqualValues(0, idx)
	s.Equal("2026/09/27/14/1967/obj.wav", e.S3Key)
	_, perr := time.Parse(time.RFC3339, e.PostedAt)
	s.NoError(perr, "posted_at must be RFC3339 so the post can be re-rendered")
}

// A re-page must not clobber a dispatch correction written between its read and its write.
func (s *DispatchSuite) TestHandleAdditionalDispatch_PreservesConcurrentDispatchCorrection() {
	slackMock := new(mockSlackPoster)
	tc := s.newClientUnderTest(slackMock, new(mockMLClient))
	tgid := talkgroupFromRadioShortCode["TAC10"].TGID
	stale := s.seedMeta(tc, tgid)

	corrected := stale
	corrected.Transcription = "fixed dispatch"
	corrected.DispatchCorrection = &TranscriptCorrection{By: "U1", At: time.Now(), Original: stale.Transcription}
	payload, _ := json.Marshal(corrected)
	s.Require().NoError(s.rdb.Set(s.ctx, fmt.Sprintf(tacMetaKeyFmt, tgid), string(payload), time.Hour).Err())

	slackMock.On("SendMessageContext", mock.Anything, "C-TEST", mock.Anything).Return("C-TEST", "ts-x", "", nil)
	parsed := &AdornedDeconstructedKey{dk: &DeconstructedKey{Talkgroup: FireDispatch1TGID, Time: time.Now()}}
	s.Require().NoError(tc.handleAdditionalDispatch(s.ctx, parsed, stubASRResponse("repage"), stale)) // stale copy passed in

	got, ok := tc.readClosureMeta(s.ctx, tgid)
	s.Require().True(ok)
	s.Equal("fixed dispatch", got.Transcription)
	s.NotNil(got.DispatchCorrection)
}

func (s *DispatchSuite) TestSweep_ClosedAlertKeepsDispatchCorrection() {
	slackMock := new(mockSlackPoster)
	tc := s.newClientUnderTest(slackMock, new(mockMLClient))
	tgid := talkgroupFromRadioShortCode["TAC10"].TGID
	meta := ClosureMeta{TGID: tgid, TACChannel: "TAC10", ThreadTS: "ts-rescue", SourceTalkgroup: FireDispatch1TGID,
		MessageTS: "ts-rescue", Transcription: "fixed dispatch",
		DispatchCorrection: &TranscriptCorrection{By: "U9", At: time.Now(), Original: "rescue tail"}}
	s.scheduleClosureFixture(tgid, time.Now().Add(-time.Second).Unix(), meta)

	slackMock.On("UpdateMessageContext", mock.Anything, "C-TEST", "ts-rescue", mock.Anything).Return("", "", "", nil).Once()
	slackMock.On("SendMessageContext", mock.Anything, "C-TEST", mock.Anything).Return("", "ts-closed", "", nil).Once()

	tc.sweepOnce(s.ctx)
	slackMock.AssertExpectations(s.T())

	// Builder-level assertion: closed-mode alert built from this meta carries the label.
	closedAt := time.Now()
	blocks := BuildRescueTrailBlocks(&RescueTrailBlocksInput{TACChannel: "TAC10", TranscriptionText: meta.Transcription,
		DispatchTGID: FireDispatch1TGID, ClosedAt: &closedAt, Correction: meta.DispatchCorrection})
	b, _ := json.Marshal(blocks)
	s.Contains(string(b), "Corrected by")
}
