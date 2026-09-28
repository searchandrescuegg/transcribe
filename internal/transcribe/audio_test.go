package transcribe

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/searchandrescuegg/transcribe/internal/ml"
	"github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

func TestAudioTitle(t *testing.T) {
	at := time.Date(2026, 9, 27, 14, 2, 11, 0, time.Local)
	assert.Equal(t, "TAC 10 · 14:02:11", audioTitle("TAC 10", at))
	assert.Equal(t, "TAC 10", audioTitle("TAC 10", time.Time{}))
}

func (s *DispatchSuite) TestAttachAudio_UploadsIntoThread() {
	slackMock := new(mockSlackPoster)
	tc := s.newClientUnderTest(slackMock, new(mockMLClient))
	tc.config.AudioAttachmentsEnabled = true
	audio := []byte("RIFF....WAVEfmt ")

	slackMock.On("UploadFileV2Context", mock.Anything, mock.MatchedBy(func(p slack.UploadFileV2Parameters) bool {
		return p.Channel == "C-TEST" && p.ThreadTimestamp == "ts-thread" &&
			p.Filename == "1967-1777832063_852162500.0-call_002.wav" &&
			p.Title == "TAC 10 · 14:02:11" && p.FileSize == len(audio) && p.Reader != nil
	})).Return(&slack.FileSummary{ID: "F1"}, nil).Once()

	tc.attachAudio(s.ctx, "ts-thread", "2026/09/27/14/1967/1967-1777832063_852162500.0-call_002.wav", "TAC 10 · 14:02:11", audio)
	slackMock.AssertExpectations(s.T())
}

func (s *DispatchSuite) TestAttachAudio_SkipsWhenDisabledEmptyOrOversize() {
	slackMock := new(mockSlackPoster)
	tc := s.newClientUnderTest(slackMock, new(mockMLClient))

	tc.attachAudio(s.ctx, "ts", "k.wav", "t", []byte("x")) // flag off (test config zero value)
	tc.config.AudioAttachmentsEnabled = true
	tc.attachAudio(s.ctx, "ts", "k.wav", "t", nil)                                     // empty audio
	tc.attachAudio(s.ctx, "", "k.wav", "t", []byte("x"))                               // no thread
	tc.attachAudio(s.ctx, "ts", "k.wav", "t", make([]byte, maxAudioAttachmentBytes+1)) // oversize

	slackMock.AssertNotCalled(s.T(), "UploadFileV2Context", mock.Anything, mock.Anything)
}

func (s *DispatchSuite) TestAttachAudio_ErrorIsSwallowedAndFilenameFallsBack() {
	slackMock := new(mockSlackPoster)
	tc := s.newClientUnderTest(slackMock, new(mockMLClient))
	tc.config.AudioAttachmentsEnabled = true
	slackMock.On("UploadFileV2Context", mock.Anything, mock.MatchedBy(func(p slack.UploadFileV2Parameters) bool {
		return p.Filename == "audio.wav"
	})).Return(nil, errors.New("slack down")).Once()

	s.NotPanics(func() { tc.attachAudio(s.ctx, "ts", "", "t", []byte("x")) })
	slackMock.AssertExpectations(s.T())
}

// Review Focus #4: post, then audio, then the (slow) summary.
func (s *DispatchSuite) TestTACPath_AttachesAudioAfterPostBeforeSummary() {
	slackMock, mlMock := new(mockSlackPoster), new(mockMLClient)
	tc := s.newClientUnderTest(slackMock, mlMock)
	tc.config.AudioAttachmentsEnabled = true
	tgid := talkgroupFromRadioShortCode["TAC10"].TGID
	s.Require().NoError(tc.dragonflyClient.Set(s.ctx, fmt.Sprintf(talkgroupKeyPrefix, tgid), time.Hour, "ts-rescue"))
	meta := ClosureMeta{TGID: tgid, TACChannel: "TAC10", ThreadTS: "ts-rescue", SourceTalkgroup: FireDispatch1TGID, MessageTS: "ts-rescue", Transcription: "Rescue Trail TAC 10"}
	b, _ := json.Marshal(meta)
	s.Require().NoError(tc.dragonflyClient.Set(s.ctx, fmt.Sprintf(tacMetaKeyFmt, tgid), time.Hour, string(b)))

	var mu sync.Mutex
	var order []string
	rec := func(step string) func(mock.Arguments) {
		return func(mock.Arguments) { mu.Lock(); order = append(order, step); mu.Unlock() }
	}
	slackMock.On("SendMessageContext", mock.Anything, "C-TEST", mock.Anything).Run(rec("post")).Return("C-TEST", "ts-post", "", nil)
	slackMock.On("UploadFileV2Context", mock.Anything, mock.MatchedBy(func(p slack.UploadFileV2Parameters) bool {
		return p.ThreadTimestamp == "ts-rescue" && p.Filename == "1967-x.wav"
	})).Run(rec("upload")).Return(&slack.FileSummary{ID: "F1"}, nil).Once()
	mlMock.On("SummarizeRescue", mock.Anything, mock.Anything).Run(rec("summary")).Return(&ml.RescueSummary{Headline: "h"}, nil).Once()

	parsed := &AdornedDeconstructedKey{dk: &DeconstructedKey{Talkgroup: tgid, Time: time.Now()}, key: "2026/09/27/14/1967/1967-x.wav"}
	s.Require().NoError(tc.processNonDispatchCall(s.ctx, parsed, stubASRResponse("on scene"), []byte("wav")))

	s.Require().GreaterOrEqual(len(order), 3)
	s.Equal([]string{"post", "upload", "summary"}, order[:3])
	slackMock.AssertExpectations(s.T())
}

// Review Focus #1: the rescue is fully registered even when the upload fails.
func (s *DispatchSuite) TestDispatchPath_AttachesAudioToAlertThreadAfterRegistration() {
	slackMock, mlMock := new(mockSlackPoster), new(mockMLClient)
	tc := s.newClientUnderTest(slackMock, mlMock)
	tc.config.AudioAttachmentsEnabled = true
	mlMock.On("ParseRelevantInformationFromDispatchMessage", mock.Anything, "raw").Return(
		dispatchMessages(ml.DispatchMessage{CallType: "Rescue - Trail", TACChannel: "TAC1", CleanedTranscription: "rescue trail call"}), nil)
	slackMock.On("SendMessageContext", mock.Anything, "C-TEST", mock.Anything).Return("C-TEST", "ts-rescue-1", "", nil).Once()

	tg := talkgroupFromRadioShortCode["TAC1"]
	slackMock.On("UploadFileV2Context", mock.Anything, mock.MatchedBy(func(p slack.UploadFileV2Parameters) bool {
		return p.ThreadTimestamp == "ts-rescue-1"
	})).Run(func(mock.Arguments) {
		// By upload time the rescue must already be registered.
		isMember, _ := tc.dragonflyClient.SMisMember(s.ctx, "allowed_talkgroups", tg.TGID)
		s.Equal([]bool{true}, isMember, "allow-listed before audio upload")
		n, _ := s.rdb.ZCard(s.ctx, activeTACsKey).Result()
		s.EqualValues(1, n, "closure scheduled before audio upload")
	}).Return(nil, errors.New("upload failed")).Once()

	parsed := &AdornedDeconstructedKey{dk: &DeconstructedKey{Talkgroup: FireDispatch1TGID, Time: time.Now()}, key: "1399-y.wav"}
	s.Require().NoError(tc.processDispatchCall(s.ctx, parsed, stubASRResponse("raw"), []byte("wav")))
	slackMock.AssertExpectations(s.T())
}

func (s *DispatchSuite) TestRepagePath_AttachesAudioToOriginalThread() {
	slackMock, mlMock := new(mockSlackPoster), new(mockMLClient)
	tc := s.newClientUnderTest(slackMock, mlMock)
	tc.config.AudioAttachmentsEnabled = true
	tgid := talkgroupFromRadioShortCode["TAC1"].TGID
	meta := ClosureMeta{TGID: tgid, TACChannel: "TAC1", ThreadTS: "orig-thread", SourceTalkgroup: FireDispatch1TGID, MessageTS: "orig-thread", Transcription: "original dispatch"}
	b, _ := json.Marshal(meta)
	s.Require().NoError(tc.dragonflyClient.Set(s.ctx, fmt.Sprintf(tacMetaKeyFmt, tgid), time.Hour, string(b)))
	s.Require().NoError(tc.dragonflyClient.Set(s.ctx, fmt.Sprintf(talkgroupKeyPrefix, tgid), time.Hour, "orig-thread"))

	mlMock.On("ParseRelevantInformationFromDispatchMessage", mock.Anything, "raw2").Return(
		dispatchMessages(ml.DispatchMessage{CallType: "Rescue - Trail", TACChannel: "TAC1", CleanedTranscription: "additional unit"}), nil)
	slackMock.On("SendMessageContext", mock.Anything, "C-TEST", mock.Anything).Return("C-TEST", "reply-ts", "", nil).Once()
	slackMock.On("UploadFileV2Context", mock.Anything, mock.MatchedBy(func(p slack.UploadFileV2Parameters) bool {
		return p.ThreadTimestamp == "orig-thread" && p.Filename == "1399-z.wav"
	})).Return(&slack.FileSummary{ID: "F2"}, nil).Once()

	parsed := &AdornedDeconstructedKey{dk: &DeconstructedKey{Talkgroup: FireDispatch1TGID, Time: time.Now()}, key: "1399-z.wav"}
	s.Require().NoError(tc.processDispatchCall(s.ctx, parsed, stubASRResponse("raw2"), []byte("wav")))
	slackMock.AssertExpectations(s.T())
}

// Review Focus #3: flag off ⇒ no upload on any path.
func (s *DispatchSuite) TestAudio_FlagOff_NoUploadOnTACPath() {
	slackMock := new(mockSlackPoster)
	tc := s.newClientUnderTest(slackMock, new(mockMLClient))
	tgid := talkgroupFromRadioShortCode["TAC1"].TGID
	s.Require().NoError(tc.dragonflyClient.Set(s.ctx, fmt.Sprintf(talkgroupKeyPrefix, tgid), time.Hour, "ts-parent"))
	slackMock.On("SendMessageContext", mock.Anything, "C-TEST", mock.Anything).Return("C-TEST", "ts-child", "", nil).Once()

	parsed := &AdornedDeconstructedKey{dk: &DeconstructedKey{Talkgroup: tgid}, key: "k.wav"}
	s.Require().NoError(tc.processNonDispatchCall(s.ctx, parsed, stubASRResponse("update"), []byte("wav")))
	slackMock.AssertNotCalled(s.T(), "UploadFileV2Context", mock.Anything, mock.Anything)
}
