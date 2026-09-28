package transcribe

import (
	"errors"
	"testing"
	"time"

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
	tc.attachAudio(s.ctx, "ts", "k.wav", "t", nil)                                    // empty audio
	tc.attachAudio(s.ctx, "", "k.wav", "t", []byte("x"))                              // no thread
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
