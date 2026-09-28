package transcribe

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/searchandrescuegg/transcribe/internal/dataset"
	"github.com/searchandrescuegg/transcribe/internal/ml"
	"github.com/slack-go/slack"
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

type capturingRecorder struct {
	mu sync.Mutex
	hc []dataset.HumanCorrectionRecord
}

func (r *capturingRecorder) RecordTranscription(dataset.TranscriptionRecord)   {}
func (r *capturingRecorder) RecordLLMInteraction(dataset.LLMInteractionRecord) {}
func (r *capturingRecorder) Close() error                                      { return nil }
func (r *capturingRecorder) RecordHumanCorrection(h dataset.HumanCorrectionRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hc = append(r.hc, h)
}

// seedActiveRescue writes tac_meta + active_tacs + one radio entry posted as ts-post-1.
func (s *DispatchSuite) seedActiveRescue(tc *TranscribeClient) (string, ClosureMeta) {
	tgid := talkgroupFromRadioShortCode["TAC10"].TGID
	meta := s.seedMeta(tc, tgid)
	s.Require().NoError(s.rdb.ZAdd(s.ctx, activeTACsKey, redisZ(time.Now().Add(30*time.Minute).Unix(), tgid)).Err())
	s.Require().NoError(tc.appendEntry(s.ctx, tgid, liveTranscriptEntry{
		CapturedAt: "14:02:11", Text: "5 year old female", Kind: entryKindRadio,
		SlackTS: "ts-post-1", S3Key: "obj.wav", PostedAt: time.Now().Format(time.RFC3339),
	}))
	return tgid, meta
}

func (s *DispatchSuite) TestResolveCorrectionTarget() {
	tc := s.newClientUnderTest(new(mockSlackPoster), new(mockMLClient))
	tgid, meta := s.seedActiveRescue(tc)

	d, err := tc.ResolveCorrectionTarget(s.ctx, meta.MessageTS, "")
	s.Require().NoError(err)
	s.Equal(CorrectionTarget{TGID: tgid, Kind: CorrectionKindDispatch, MessageTS: meta.MessageTS, ThreadTS: meta.ThreadTS, CurrentText: meta.Transcription}, d)

	r, err := tc.ResolveCorrectionTarget(s.ctx, "ts-post-1", meta.ThreadTS)
	s.Require().NoError(err)
	s.Equal(CorrectionKindTAC, r.Kind)
	s.Equal("5 year old female", r.CurrentText)

	_, err = tc.ResolveCorrectionTarget(s.ctx, "ts-live-interp", meta.ThreadTS)
	s.ErrorIs(err, ErrNotCorrectable)

	_, err = tc.ResolveCorrectionTarget(s.ctx, "ts-x", "ts-unknown-thread")
	s.ErrorIs(err, ErrRescueNotActive)
}

func (s *DispatchSuite) TestApplyTranscriptCorrection_TAC_UpdatesEntryLabelsPostAndRecords() {
	slackMock := new(mockSlackPoster)
	tc := s.newClientUnderTest(slackMock, new(mockMLClient))
	rec := &capturingRecorder{}
	tc.recorder = rec
	tgid, meta := s.seedActiveRescue(tc)
	target, err := tc.ResolveCorrectionTarget(s.ctx, "ts-post-1", meta.ThreadTS)
	s.Require().NoError(err)

	slackMock.On("UpdateMessageContext", mock.Anything, "C-TEST", "ts-post-1", mock.Anything).Return("", "", "", nil).Once()

	s.Require().NoError(tc.ApplyTranscriptCorrection(s.ctx, target, "U1", "  54 year old female ", time.Now()))
	slackMock.AssertExpectations(s.T())

	_, e, found, err := tc.findEntryBySlackTS(s.ctx, tgid, "ts-post-1", entryKindRadio)
	s.Require().NoError(err)
	s.Require().True(found)
	s.Equal("54 year old female", e.effectiveText())
	s.Equal("U1", e.Correction.By)
	s.Equal("5 year old female", e.Correction.Original)

	s.Require().Len(rec.hc, 1)
	s.Equal(dataset.HumanCorrectionRecord{Kind: dataset.HumanCorrectionKindTAC, Action: dataset.HumanCorrectionActionCreate,
		TGID: tgid, S3Key: "obj.wav", SlackTS: "ts-post-1", SlackUserID: "U1",
		PriorText: "5 year old female", CorrectedText: "54 year old female"}, rec.hc[0])

	// One correction only.
	_, err = tc.ResolveCorrectionTarget(s.ctx, "ts-post-1", meta.ThreadTS)
	var already *AlreadyCorrectedError
	s.Require().ErrorAs(err, &already)
	s.Equal("U1", already.Correction.By)
}

// Review Focus #3: concurrent submissions — exactly one wins.
func (s *DispatchSuite) TestApplyTranscriptCorrection_ConcurrentSubmissions_OneWins() {
	slackMock := new(mockSlackPoster)
	tc := s.newClientUnderTest(slackMock, new(mockMLClient))
	_, meta := s.seedActiveRescue(tc)
	target, err := tc.ResolveCorrectionTarget(s.ctx, "ts-post-1", meta.ThreadTS)
	s.Require().NoError(err)
	slackMock.On("UpdateMessageContext", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return("", "", "", nil)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, text := range []string{"alpha", "bravo"} {
		wg.Add(1)
		go func(i int, text string) {
			defer wg.Done()
			errs[i] = tc.ApplyTranscriptCorrection(s.ctx, target, fmt.Sprintf("U%d", i), text, time.Now())
		}(i, text)
	}
	wg.Wait()

	var already *AlreadyCorrectedError
	wins := 0
	for _, e := range errs {
		if e == nil {
			wins++
		} else {
			s.True(errors.As(e, &already), "loser must get AlreadyCorrectedError, got %v", e)
		}
	}
	s.Equal(1, wins)
}

func (s *DispatchSuite) TestApplyTranscriptCorrection_Dispatch_UpdatesMetaAndAlert() {
	slackMock := new(mockSlackPoster)
	tc := s.newClientUnderTest(slackMock, new(mockMLClient))
	tgid, meta := s.seedActiveRescue(tc)
	target, err := tc.ResolveCorrectionTarget(s.ctx, meta.MessageTS, meta.ThreadTS)
	s.Require().NoError(err)
	slackMock.On("UpdateMessageContext", mock.Anything, "C-TEST", meta.MessageTS, mock.Anything).Return("", "", "", nil).Once()

	s.Require().NoError(tc.ApplyTranscriptCorrection(s.ctx, target, "U2", "Rescue Trail TAC 10 Mailbox Peak", time.Now()))
	slackMock.AssertExpectations(s.T())

	got, ok := tc.readClosureMeta(s.ctx, tgid)
	s.Require().True(ok)
	s.Equal("Rescue Trail TAC 10 Mailbox Peak", got.Transcription)
	s.Require().NotNil(got.DispatchCorrection)
	s.Equal(meta.Transcription, got.DispatchCorrection.Original)
}

// tac_meta can be deleted between ResolveCorrectionTarget and ApplyTranscriptCorrection by a
// sweeper close, a Cancel, or a Switch racing the correction. SetXX must refuse to resurrect
// it (rather than writing a zombie key with none of active_tacs / allowed_talkgroups /
// tg:<TGID> behind it), and the one-shot guard must be released so the request isn't bricked.
func (s *DispatchSuite) TestApplyTranscriptCorrection_Dispatch_TacMetaDeletedConcurrently_ReturnsErrRescueNotActive() {
	tc := s.newClientUnderTest(new(mockSlackPoster), new(mockMLClient))
	tgid, meta := s.seedActiveRescue(tc)
	target, err := tc.ResolveCorrectionTarget(s.ctx, meta.MessageTS, meta.ThreadTS)
	s.Require().NoError(err)

	s.Require().NoError(s.rdb.Del(s.ctx, fmt.Sprintf(tacMetaKeyFmt, tgid)).Err())

	s.ErrorIs(tc.ApplyTranscriptCorrection(s.ctx, target, "U1", "new text", time.Now()), ErrRescueNotActive)

	exists, err := s.rdb.Exists(s.ctx, fmt.Sprintf(tacMetaKeyFmt, tgid)).Result()
	s.Require().NoError(err)
	s.EqualValues(0, exists, "SetXX must not resurrect a deleted tac_meta key")

	exists, err = s.rdb.Exists(s.ctx, fmt.Sprintf(correctedGuardKeyFmt, meta.MessageTS)).Result()
	s.Require().NoError(err)
	s.EqualValues(0, exists, "guard must be released so a retry isn't bricked")
}

func (s *DispatchSuite) TestApplyTranscriptCorrection_ValidationAndGuardRelease() {
	tc := s.newClientUnderTest(new(mockSlackPoster), new(mockMLClient))
	_, meta := s.seedActiveRescue(tc)
	target, err := tc.ResolveCorrectionTarget(s.ctx, "ts-post-1", meta.ThreadTS)
	s.Require().NoError(err)

	s.ErrorIs(tc.ApplyTranscriptCorrection(s.ctx, target, "U1", "   ", time.Now()), ErrEmptyCorrection)
	s.ErrorIs(tc.ApplyTranscriptCorrection(s.ctx, target, "U1", "5 year old female", time.Now()), ErrUnchangedCorrection)

	// Entry vanished (e.g. Switch reset the list) → guard released so the message isn't bricked.
	s.Require().NoError(s.rdb.Del(s.ctx, fmt.Sprintf(tacTranscriptsKeyFmt, target.TGID)).Err())
	s.ErrorIs(tc.ApplyTranscriptCorrection(s.ctx, target, "U1", "new", time.Now()), ErrNotCorrectable)
	exists, err := s.rdb.Exists(s.ctx, fmt.Sprintf(correctedGuardKeyFmt, "ts-post-1")).Result()
	s.Require().NoError(err)
	s.EqualValues(0, exists)
}

func redisZ(score int64, member string) redis.Z {
	return redis.Z{Score: float64(score), Member: member}
}

func (s *DispatchSuite) TestOperatorCorrections_CreateEditRemove() {
	tc := s.newClientUnderTest(new(mockSlackPoster), new(mockMLClient))
	rec := &capturingRecorder{}
	tc.recorder = rec
	tgid, meta := s.seedActiveRescue(tc)
	at := time.Date(2026, 9, 27, 14, 5, 12, 0, time.Local)

	got, created, err := tc.UpsertOperatorCorrection(s.ctx, meta.ThreadTS, "ts-note", "U1", "diabetic per family", at)
	s.Require().NoError(err)
	s.Equal(tgid, got)
	s.True(created)

	_, created, err = tc.UpsertOperatorCorrection(s.ctx, meta.ThreadTS, "ts-note", "U1", "diabetic, insulin in pack", at)
	s.Require().NoError(err)
	s.False(created, "same slack ts = edit")

	entries, err := tc.readEntries(s.ctx, tgid)
	s.Require().NoError(err)
	s.Len(entries, 2, "edit amends in place, no new entry")
	in := buildSummaryInput(meta, entries, nil, "")
	s.Equal([]ml.OperatorCorrection{{At: "14:05:12", Text: "diabetic, insulin in pack"}}, in.OperatorCorrections)

	_, removed, err := tc.RemoveOperatorCorrection(s.ctx, meta.ThreadTS, "ts-note", "U1")
	s.Require().NoError(err)
	s.True(removed)
	entries, _ = tc.readEntries(s.ctx, tgid)
	s.Len(entries, 2, "retraction is a tombstone; indexes stay stable")
	s.Empty(buildSummaryInput(meta, entries, nil, "").OperatorCorrections)

	_, removed, err = tc.RemoveOperatorCorrection(s.ctx, meta.ThreadTS, "ts-never-stored", "U1")
	s.Require().NoError(err)
	s.False(removed)

	s.Require().Len(rec.hc, 3)
	s.Equal([]string{dataset.HumanCorrectionActionCreate, dataset.HumanCorrectionActionEdit, dataset.HumanCorrectionActionDelete},
		[]string{rec.hc[0].Action, rec.hc[1].Action, rec.hc[2].Action})
	s.Equal("diabetic per family", rec.hc[1].PriorText)
}

func (s *DispatchSuite) TestOperatorCorrections_NoActiveRescue() {
	tc := s.newClientUnderTest(new(mockSlackPoster), new(mockMLClient))
	_, _, err := tc.UpsertOperatorCorrection(s.ctx, "ts-nothing", "ts-note", "U1", "x", time.Now())
	s.ErrorIs(err, ErrRescueNotActive)
}

func (s *DispatchSuite) TestMigrateOperatorCorrections_CopiesLiveNotesOnly() {
	tc := s.newClientUnderTest(new(mockSlackPoster), new(mockMLClient))
	oldTGID, meta := s.seedActiveRescue(tc)
	_, _, err := tc.UpsertOperatorCorrection(s.ctx, meta.ThreadTS, "ts-a", "U1", "keep me", time.Now())
	s.Require().NoError(err)
	_, _, err = tc.UpsertOperatorCorrection(s.ctx, meta.ThreadTS, "ts-b", "U1", "retracted", time.Now())
	s.Require().NoError(err)
	_, _, err = tc.RemoveOperatorCorrection(s.ctx, meta.ThreadTS, "ts-b", "U1")
	s.Require().NoError(err)

	newTGID := talkgroupFromRadioShortCode["TAC8"].TGID
	s.Require().NoError(tc.MigrateOperatorCorrections(s.ctx, oldTGID, newTGID))

	entries, err := tc.readEntries(s.ctx, newTGID)
	s.Require().NoError(err)
	s.Require().Len(entries, 1, "radio entries and tombstones are not migrated")
	s.Equal("keep me", entries[0].Text)
	s.Equal("ts-a", entries[0].SlackTS, "slack ts preserved so later edits/deletes still resolve")
}

// Human-typed dispatch text must never reach CleanTACTranscript (cleanup is phonetic-only and
// must fail safe). After a dispatch correction the cleanup context is the ORIGINAL ASR text,
// while CAD correlation keeps the corrected text.
func (s *DispatchSuite) TestMaybeCleanTranscript_DispatchCorrected_UsesOriginalASRAsContext() {
	mlMock := new(mockMLClient)
	tc := s.newClientUnderTest(new(mockSlackPoster), mlMock)
	tc.config.TACCleanupEnabled = true
	tgid := talkgroupFromRadioShortCode["TAC10"].TGID
	meta := ClosureMeta{TGID: tgid, TACChannel: "TAC10", ThreadTS: "ts-rescue", SourceTalkgroup: FireDispatch1TGID,
		MessageTS: "ts-rescue", Transcription: "Rescue Trail TAC 10 Mailbox Peak (human typed)",
		DispatchCorrection: &TranscriptCorrection{By: "U1", At: time.Now(), Original: "rescue trail tack ten mailbox peek"}}
	payload, _ := json.Marshal(meta)
	s.Require().NoError(tc.dragonflyClient.Set(s.ctx, fmt.Sprintf(tacMetaKeyFmt, tgid), time.Hour, string(payload)))

	mlMock.On("CleanTACTranscript", mock.Anything, mock.MatchedBy(func(in ml.TACCleanupInput) bool {
		return in.DispatchContext == meta.DispatchCorrection.Original
	})).Return(&ml.TACCleanupResult{CleanedText: "cleaned"}, nil).Once()

	s.Equal("cleaned", tc.maybeCleanTranscript(s.ctx, tgid, "raw"))
	mlMock.AssertExpectations(s.T())
}

// A summary pass can take ~120s in the LLM. If a dispatch correction lands mid-pass, the SAR
// badge re-render must use the FRESH tac_meta — rendering the stale copy would permanently wipe
// the (one-shot) correction off the parent alert.
func (s *DispatchSuite) TestSARBadge_UsesFreshMetaAfterMidPassDispatchCorrection() {
	slackMock, mlMock := new(mockSlackPoster), new(mockMLClient)
	tc := s.newClientUnderTest(slackMock, mlMock)
	tgid, meta := s.seedActiveRescue(tc)
	s.Require().NoError(s.rdb.Set(s.ctx, fmt.Sprintf(summaryTSKeyFmt, tgid), "ts-sum", time.Hour).Err())

	const corrected = "Rescue Trail TAC 10 Mailbox Peak"
	mlMock.On("SummarizeRescue", mock.Anything, mock.Anything).Run(func(mock.Arguments) {
		fresh := meta
		fresh.Transcription = corrected
		fresh.DispatchCorrection = &TranscriptCorrection{By: "U7", At: time.Now(), Original: meta.Transcription}
		b, _ := json.Marshal(fresh)
		s.Require().NoError(s.rdb.Set(s.ctx, fmt.Sprintf(tacMetaKeyFmt, tgid), string(b), time.Hour).Err())
	}).Return(&ml.RescueSummary{Headline: "h", SARNotified: true}, nil).Once()

	var alertBlocks string
	slackMock.On("UpdateMessageContext", mock.Anything, "C-TEST", meta.MessageTS, mock.Anything).Run(func(args mock.Arguments) {
		_, vals, err := slack.UnsafeApplyMsgOptions("", "C-TEST", "", args.Get(3).([]slack.MsgOption)...)
		s.Require().NoError(err)
		alertBlocks = vals.Get("blocks")
	}).Return("", "", "", nil).Once()
	slackMock.On("UpdateMessageContext", mock.Anything, "C-TEST", "ts-sum", mock.Anything).Return("", "", "", nil).Once()

	s.True(tc.runOneSummaryPass(s.ctx, tgid, false))
	slackMock.AssertExpectations(s.T())
	s.Contains(alertBlocks, corrected, "parent alert must render the corrected dispatch text")
	s.Contains(alertBlocks, "Corrected by", "parent alert must keep the correction label")
	s.Contains(alertBlocks, "Rescue notified", "SAR badge still applied")
}

// Slack re-sends message_changed for link unfurls etc. with identical text: a no-op, not an edit.
func (s *DispatchSuite) TestOperatorCorrections_UnchangedEdit_IsNoOp() {
	tc := s.newClientUnderTest(new(mockSlackPoster), new(mockMLClient))
	rec := &capturingRecorder{}
	tc.recorder = rec
	tgid, meta := s.seedActiveRescue(tc)

	_, _, err := tc.UpsertOperatorCorrection(s.ctx, meta.ThreadTS, "ts-note", "U1", "diabetic", time.Now())
	s.Require().NoError(err)
	before, err := s.rdb.LRange(s.ctx, fmt.Sprintf(tacTranscriptsKeyFmt, tgid), 0, -1).Result()
	s.Require().NoError(err)

	got, created, err := tc.UpsertOperatorCorrection(s.ctx, meta.ThreadTS, "ts-note", "U2", "diabetic", time.Now())
	s.ErrorIs(err, ErrOperatorCorrectionUnchanged)
	s.Equal(tgid, got)
	s.False(created)

	after, err := s.rdb.LRange(s.ctx, fmt.Sprintf(tacTranscriptsKeyFmt, tgid), 0, -1).Result()
	s.Require().NoError(err)
	s.Equal(before, after, "entry untouched")
	s.Len(rec.hc, 1, "no dataset row for an unchanged edit")

	// A retracted note re-edited back to the same text is a real change (resurrection).
	_, _, err = tc.RemoveOperatorCorrection(s.ctx, meta.ThreadTS, "ts-note", "U1")
	s.Require().NoError(err)
	_, _, err = tc.UpsertOperatorCorrection(s.ctx, meta.ThreadTS, "ts-note", "U1", "diabetic", time.Now())
	s.NoError(err)
}
