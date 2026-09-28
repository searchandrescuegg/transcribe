# Audio Attachments Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Upload each transcription's WAV as a file reply in the rescue thread, right after its transcript post, for TAC transmissions, the dispatch alert, and re-page replies.

**Architecture:** `SlackPoster` gains slack-go's `UploadFileV2Context`. A new best-effort helper `attachAudio` in `internal/transcribe/audio.go` uploads bytes into a thread, gated on `AUDIO_ATTACHMENTS_ENABLED`. `processRecord` passes the WAV bytes it already downloaded for ASR into the dispatch, TAC, and re-page paths. Each path uploads right after its post succeeds, and before the summary LLM call on the TAC path.

**Tech Stack:** Go 1.25, `slack-go/slack` v0.17.1 (`UploadFileV2Context`, `UploadFileV2Parameters`, `FileSummary`), `caarlos0/env/v11`, `testify`, `testcontainers-go`.

**Spec:** `docs/superpowers/specs/2026-09-28-audio-attachments-design.md`

## Global Constraints

- Branch `feat/audio-attachments` (stacked on `feat/alert-brief`); do not switch branches.
- Commits are GPG-signed automatically (`commit.gpgsign=true`). Never pass `--no-gpg-sign`, and never rewrite history.
- Commit subjects must be conventional, start with a lowercase word, and be ≤ 100 chars (CI runs commitlint `config-conventional`). Every commit message ends with exactly:
  ```
  Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01UGf82rADsPjLxqirG637Y2
  ```
- Flag: `AudioAttachmentsEnabled bool \`env:"AUDIO_ATTACHMENTS_ENABLED" envDefault:"true"\``.
- Upload timeout is 20s (`audioUploadTimeout`), and uploads are capped at 20 MiB (`maxAudioAttachmentBytes = 20 << 20`).
- Filename is `path.Base(s3Key)`, falling back to `"audio.wav"`. Title is `"<talkgroup FullName> · 15:04:05"` in local time, or just the name when the time is zero.
- Uploads are best-effort. An upload failure logs a WARN and never fails or nacks the record.
- The upload always happens AFTER the transcript/alert post succeeds. It must never delay allow-listing, routing, or `ScheduleTACClosure` on the dispatch path.
- Do not commit the untracked `grafana/` directory or the `./transcribe` binary.

## Review Focus

1. Upload failure or timeout on the dispatch path. The rescue must already be fully registered (allow-list, routing, closure), and the call must return nil. (Task 2)
2. Audio bytes empty (a nil pass-through, e.g. from tests or a future caller): no upload, no panic. (Task 1)
3. Feature flag off: zero `UploadFileV2Context` calls on every path. (Tasks 1, 2)
4. TAC ordering. The audio reply must follow its own transcript post and precede the long summary call, so the audio isn't delayed behind the LLM. (Task 2)
5. An S3 key with a date/talkgroup prefix (`2026/09/27/14/1967/1967-…wav`). The filename must be just the basename. (Task 1)

---

### Task 1: Upload helper, flag, and SlackPoster method

**Files:**
- Modify: `internal/config/config.go` (next to `TACCleanupEnabled`, ~line 95)
- Modify: `internal/transcribe/transcribe.go` (`SlackPoster` interface, ~line 45)
- Modify: `internal/transcribe/integration_test.go` (`mockSlackPoster`, ~line 38)
- Create: `internal/transcribe/audio.go`, `internal/transcribe/audio_test.go`

**Interfaces:**
- Produces: `config.Config.AudioAttachmentsEnabled`; `SlackPoster.UploadFileV2Context(ctx context.Context, params slack.UploadFileV2Parameters) (*slack.FileSummary, error)`; `func (tc *TranscribeClient) attachAudio(ctx context.Context, threadTS, s3Key, title string, audio []byte)`; `func audioTitle(channelName string, capturedAt time.Time) string`; consts `audioUploadTimeout`, `maxAudioAttachmentBytes`.

- [ ] **Step 1: Write the failing tests** (create `internal/transcribe/audio_test.go`)

```go
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
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -count=1 ./internal/transcribe/ -run 'TestAudioTitle|TestDispatchSuite/TestAttachAudio'`
Expected: compile FAIL (`undefined: audioTitle`, `AudioAttachmentsEnabled`, mock missing `UploadFileV2Context`).

- [ ] **Step 3: Implement**

`internal/config/config.go`, after `TACCleanupEnabled`:

```go
	// AudioAttachmentsEnabled uploads each transcription's WAV as a file reply in the rescue
	// thread (TAC transmissions, the dispatch alert, re-page replies) so responders can listen to
	// garbled audio. Best-effort; kill switch for thread noise or Slack upload limits. Requires
	// the files:write bot scope.
	AudioAttachmentsEnabled bool `env:"AUDIO_ATTACHMENTS_ENABLED" envDefault:"true"`
```

`internal/transcribe/transcribe.go` `SlackPoster`, add (and extend the interface comment with one line saying uploads carry transcription audio):

```go
	UploadFileV2Context(ctx context.Context, params slack.UploadFileV2Parameters) (*slack.FileSummary, error)
```

`internal/transcribe/integration_test.go`, next to the other `mockSlackPoster` methods:

```go
func (m *mockSlackPoster) UploadFileV2Context(ctx context.Context, params slack.UploadFileV2Parameters) (*slack.FileSummary, error) {
	args := m.Called(ctx, params)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*slack.FileSummary), args.Error(1)
}
```

Create `internal/transcribe/audio.go`:

```go
package transcribe

import (
	"bytes"
	"context"
	"log/slog"
	"path"
	"time"

	"github.com/slack-go/slack"
)

const (
	// audioUploadTimeout bounds one upload (URL fetch + PUT + complete) so a slow Slack upload
	// can't eat the worker's budget; it sits well inside WorkerTimeout.
	audioUploadTimeout = 20 * time.Second
	// maxAudioAttachmentBytes skips pathological files; trunk-recorder calls are far smaller.
	maxAudioAttachmentBytes = 20 << 20
)

// audioTitle labels an audio reply so it reads as belonging to the post above it.
func audioTitle(channelName string, capturedAt time.Time) string {
	if capturedAt.IsZero() {
		return channelName
	}
	return channelName + " · " + capturedAt.Local().Format("15:04:05")
}

// attachAudio uploads a transcription's audio as a file reply in threadTS so responders can
// listen to what the ASR heard. Best-effort by design: the transcript post already succeeded,
// so any failure logs and returns — it must never fail or nack the record.
func (tc *TranscribeClient) attachAudio(ctx context.Context, threadTS, s3Key, title string, audio []byte) {
	if !tc.config.AudioAttachmentsEnabled || threadTS == "" || len(audio) == 0 {
		return
	}
	if len(audio) > maxAudioAttachmentBytes {
		slog.Info("audio attachment skipped: file too large",
			slog.String("key", s3Key), slog.Int("bytes", len(audio)))
		return
	}
	filename := path.Base(s3Key)
	if s3Key == "" || filename == "." || filename == "/" {
		filename = "audio.wav"
	}
	uploadCtx, cancel := context.WithTimeout(ctx, audioUploadTimeout)
	defer cancel()
	if _, err := tc.slackClient.UploadFileV2Context(uploadCtx, slack.UploadFileV2Parameters{
		Channel:         tc.config.SlackChannelID,
		ThreadTimestamp: threadTS,
		Reader:          bytes.NewReader(audio),
		FileSize:        len(audio),
		Filename:        filename,
		Title:           title,
	}); err != nil {
		slog.Warn("audio attachment failed",
			slog.String("error", err.Error()), slog.String("key", s3Key), slog.String("thread", threadTS))
	}
}
```

- [ ] **Step 4: Run tests to verify pass**

Run: `gofmt -l $(git ls-files '*.go'); go build ./... && go vet ./internal/... && go test -count=1 ./internal/transcribe/... ./internal/config/...`
Expected: PASS, and gofmt prints nothing. `go build ./...` proves `*slack.Client` still satisfies `SlackPoster` in `cmd/transcribe/main.go`.

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/transcribe/transcribe.go internal/transcribe/integration_test.go internal/transcribe/audio.go internal/transcribe/audio_test.go
git commit -F - <<'EOF'
feat(transcribe): add best-effort audio upload helper behind a flag

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01UGf82rADsPjLxqirG637Y2
EOF
```

---

### Task 2: Pass audio through and attach on all three paths

**Files:**
- Modify: `internal/transcribe/transcribe.go` (`processRecord` calls, ~line 318-327)
- Modify: `internal/transcribe/process.go` (`processDispatchCall`, `handleAdditionalDispatch`, `processNonDispatchCall`)
- Modify: every existing test call of these three functions (grep `processDispatchCall(\|processNonDispatchCall(\|handleAdditionalDispatch(` in `internal/transcribe/*_test.go`; about 15 sites), adding a trailing `nil`
- Test: `internal/transcribe/audio_test.go`

**Interfaces:**
- Consumes: Task 1 `attachAudio`, `audioTitle`, `talkgroupFromTGID`, `FireDispatch1TGID`, `AdornedDeconstructedKey.key`.
- Produces: `processDispatchCall(ctx, parsedKey, tr, audio []byte) error`, `processNonDispatchCall(ctx, parsedKey, tr, audio []byte) error`, `handleAdditionalDispatch(ctx, parsedKey, tr, meta ClosureMeta, audio []byte) error`.

- [ ] **Step 1: Write the failing tests** (append to `internal/transcribe/audio_test.go`; add imports `encoding/json`, `fmt`, `sync`, `github.com/searchandrescuegg/transcribe/internal/ml`)

```go
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
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -count=1 -run 'TestDispatchSuite/(TestTACPath|TestDispatchPath|TestRepagePath|TestAudio_FlagOff)' ./internal/transcribe/...`
Expected: compile FAIL (too many arguments to processNonDispatchCall/processDispatchCall).

- [ ] **Step 3: Implement**

`transcribe.go` `processRecord`: change the two calls to pass `fileBytes`:

```go
		if err := tc.processDispatchCall(ctx, parsedKey, tr, fileBytes); err != nil {
```
```go
	if err := tc.processNonDispatchCall(ctx, parsedKey, tr, fileBytes); err != nil {
```

`process.go`:
- Signatures: `func (tc *TranscribeClient) processDispatchCall(ctx context.Context, parsedKey *AdornedDeconstructedKey, tr *asr.TranscriptionResponse, audio []byte) error`; same trailing `audio []byte` on `processNonDispatchCall`; `handleAdditionalDispatch(ctx, parsedKey, tr, meta ClosureMeta, audio []byte) error`.
- In `processDispatchCall`, the re-page branch becomes `return tc.handleAdditionalDispatch(ctx, parsedKey, tr, meta, audio)`.
- In `processDispatchCall`, immediately BEFORE the CAD warm-up block (`if tc.unitResolver != nil { tc.resolveAndCacheUnitContext(...) }`), which comes after `ScheduleTACClosure`, insert:

```go
	// Attach the dispatch audio as the first reply in the alert thread. Only after the rescue is
	// fully registered (allow-list, routing, closure) so a slow/failed upload can never delay or
	// block monitoring. Best-effort.
	tc.attachAudio(ctx, tsThread, parsedKey.key, audioTitle(dispatchChannelName(), parsedKey.dk.Time), audio)
```

- In `handleAdditionalDispatch`, after the thread-reply `sendSlackWithRetry` block (before `return nil`):

```go
	tc.attachAudio(ctx, meta.ThreadTS, parsedKey.key, audioTitle(dispatchChannelName(), parsedKey.dk.Time), audio)
```

- In `processNonDispatchCall`, after the post-success debug log and BEFORE `tc.appendAndRefresh(...)`:

```go
	// Audio right under its transcript, before the (slow) summary refresh. Best-effort.
	tc.attachAudio(ctx, tsThread, parsedKey.key, audioTitle(tgInfo.FullName, parsedKey.dk.Time), audio)
```

- Add at the bottom of `process.go`:

```go
// dispatchChannelName is the display name used to title Fire Dispatch 1 audio replies.
func dispatchChannelName() string {
	if tg, ok := talkgroupFromTGID[FireDispatch1TGID]; ok {
		return tg.FullName
	}
	return "Fire Dispatch 1"
}
```

- Update every existing test call site of the three functions to pass a trailing `nil` (grep given under Files).

- [ ] **Step 4: Run tests to verify pass**

Run: `gofmt -l $(git ls-files '*.go'); go vet ./... && go test -count=1 ./internal/transcribe/...`
Expected: PASS, and gofmt prints nothing.

- [ ] **Step 5: Commit**

```bash
git add internal/transcribe/
git commit -F - <<'EOF'
feat(transcribe): attach transcription audio to tac, dispatch and re-page posts

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01UGf82rADsPjLxqirG637Y2
EOF
```

---

### Task 3: Slack scope, env example, docs, and full verification

**Files:**
- Modify: `slack/manifest.yaml` (bot scopes list)
- Modify: `README.md` (Slack setup section; the human-corrections item 6 is the model for wording)
- Modify: `.env.example` (next to `# TAC_CLEANUP_ENABLED=true`, ~line 75)
- Modify: `CLAUDE.md` (architecture table; file map)

- [ ] **Step 1: Manifest**: add to `oauth_config.scopes.bot`, with a comment in the file's style:

```yaml
      # files:write — upload each transcription's audio (WAV) as a reply in the rescue thread so
      # responders can listen to what the ASR heard. Gated by AUDIO_ATTACHMENTS_ENABLED.
      - files:write
```

Validate: `python3 -c "import yaml; yaml.safe_load(open('slack/manifest.yaml'))"`.

- [ ] **Step 2: README**: in the Slack setup list, add an item after the human-corrections item:

```markdown
7. **Audio attachments** — each transcription's WAV is posted as a reply right under its post
   (TAC transmissions, the dispatch alert, re-pages) so you can listen when the transcript looks
   wrong. Needs the `files:write` scope (in the manifest) — reinstall the app, then compare
   **OAuth & Permissions → Bot User OAuth Token** with `SLACK_TOKEN` and update it if Slack
   issued a new one. Turn off with `AUDIO_ATTACHMENTS_ENABLED=false`.
```

- [ ] **Step 3: `.env.example`**, next to the TAC cleanup flag, in the same commented style:

```
# Upload each transcription's audio as a reply in the rescue thread (needs files:write).
# AUDIO_ATTACHMENTS_ENABLED=true
```

- [ ] **Step 4: CLAUDE.md**
- Architecture table row:

```markdown
| Transcription audio as a threaded file reply | Responders can listen when the ASR is garbled. The transcript post is unchanged (correction targeting keys on its ts); the WAV is uploaded as a separate reply right after it via `UploadFileV2Context`, using bytes `processRecord` already fetched for ASR. Best-effort (never fails/nacks the record), flag `AUDIO_ATTACHMENTS_ENABLED`, `files:write` scope. Dispatch-alert audio uploads only after the rescue is fully registered; TAC audio uploads before the summary LLM call | `internal/transcribe/audio.go` (`attachAudio`), `process.go` (call sites) |
```

- File map row: `| internal/transcribe/audio.go | attachAudio / audioTitle — best-effort threaded WAV upload |`.

- [ ] **Step 5: Full verification**

Run: `gofmt -l $(git ls-files '*.go'); go vet ./... && go test -count=1 -timeout 5m ./...`
Expected: gofmt prints nothing, and every package passes.

- [ ] **Step 6: Commit**

```bash
git add slack/manifest.yaml README.md .env.example CLAUDE.md
git commit -F - <<'EOF'
docs: add files:write scope and document audio attachments

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01UGf82rADsPjLxqirG637Y2
EOF
```
