# Audio attachments on transcription posts — design

Status: approved in design review 2026-09-28; spec pending review.
Sub-project 3 of 3 (1 = human corrections, PR #27; 2 = alert brief, PR #28).
Branch: `feat/audio-attachments`, stacked on `feat/alert-brief`.

## Goal

Let people listen to the exact audio behind each transcription ("if we read 'rouse the mountain
rescue command' I can listen and try to figure it out myself").

## Decisions

| Question | Decision |
|---|---|
| Shape | Transcript post stays exactly as today; the WAV is uploaded as its own small file reply in the same thread immediately after. Two thread messages per transmission. Correction targeting (sub-project 1) is unchanged — it keys on the transcript post's ts. |
| Coverage | TAC transmissions, the dispatch alert (as a reply in the alert's thread), and re-page dispatch replies. |
| Switch | `AUDIO_ATTACHMENTS_ENABLED` env flag, default `true`. |
| Audio source | Pass the WAV bytes `processRecord` already downloaded for ASR; no second S3 read. |
| Failure | Best-effort: an upload failure logs and never fails / nacks the record. |
| Out of scope | Transcoding (WAV → MP3), audio on the Live Interpretation, audio for rescues already posted before deploy. |

## Components

### `SlackPoster` (internal/transcribe/transcribe.go)

Add `UploadFileV2Context(ctx context.Context, params slack.UploadFileV2Parameters) (*slack.FileSummary, error)`.
`*slack.Client` (slack-go v0.17.1) already has this exact method, so production wiring is
unchanged. The test `mockSlackPoster` (integration_test.go) gains the method.

### `internal/transcribe/audio.go` (new)

```go
const (
    audioUploadTimeout = 20 * time.Second
    maxAudioAttachmentBytes = 20 << 20 // 20 MiB — trunk-recorder calls are far smaller
)

// attachAudio uploads audio as a file reply in threadTS. Best-effort: never returns an error.
func (tc *TranscribeClient) attachAudio(ctx context.Context, threadTS, s3Key, title string, audio []byte)
```

- No-op when `!tc.config.AudioAttachmentsEnabled`, `threadTS == ""`, or `len(audio) == 0`.
- Skips (Info log) when `len(audio) > maxAudioAttachmentBytes`.
- `UploadFileV2Context` with `Channel: tc.config.SlackChannelID`, `ThreadTimestamp: threadTS`,
  `Reader: bytes.NewReader(audio)`, `FileSize: len(audio)`, `Filename: path.Base(s3Key)`
  (fallback `"audio.wav"` when empty), `Title: title`, under `context.WithTimeout(ctx, audioUploadTimeout)`.
- On error: `slog.Warn("audio attachment failed", …)` with key, thread, error; return.
- Title helper: `audioTitle(channelName string, capturedAt time.Time) string` →
  `"<channelName> · 15:04:05"` in local time; `"<channelName>"` when `capturedAt` is zero.

### Plumbing the bytes

- `processRecord` passes `fileBytes` to `processDispatchCall(ctx, parsedKey, tr, fileBytes)` and
  `processNonDispatchCall(ctx, parsedKey, tr, fileBytes)`; `processDispatchCall` passes it to
  `handleAdditionalDispatch(ctx, parsedKey, tr, meta, audio)`. All existing test call sites add
  a `nil` argument.

### Call sites

- **TAC** (`processNonDispatchCall`): after the transcript post succeeds (post ts known), call
  `attachAudio(ctx, tsThread, parsedKey.key, audioTitle(tgInfo.FullName, parsedKey.dk.Time), audio)`,
  then `appendAndRefresh` (summary). Audio lands right under its post instead of after the LLM.
- **Dispatch alert** (`processDispatchCall`): after the alert post, routing key, and
  `ScheduleTACClosure` — i.e. after the rescue is fully registered — call
  `attachAudio(ctx, tsThread, parsedKey.key, audioTitle(<Fire Dispatch 1 full name>, parsedKey.dk.Time), audio)`.
  Must not run before the alert is posted or delay allow-listing.
- **Re-page** (`handleAdditionalDispatch`): after the thread reply posts, same title scheme, thread
  `meta.ThreadTS`.
- Uploads are synchronous within the worker (ordering within a worker follows posts; across
  workers matches existing post ordering). Bounded by `audioUploadTimeout` inside `WorkerTimeout`.

### Interactions

- "Correct transcript" on an audio reply → `ResolveCorrectionTarget` → `ErrNotCorrectable`
  ("Only dispatch and radio transcription posts can be corrected"). No change needed.
- `correction:` detection ignores bot messages; audio replies are bot-authored.
- No Dragonfly keys, no dataset changes, no invariant changes beyond documenting the ordering.

## Config / Slack app

- `internal/config/config.go`: `AudioAttachmentsEnabled bool \`env:"AUDIO_ATTACHMENTS_ENABLED" envDefault:"true"\``
  with a comment (thread noise / Slack-limit kill switch). Add to `.env.example` if it lists flags.
- `slack/manifest.yaml`: bot scope `files:write` (comment: upload transcription audio into the
  rescue thread). README: note the added scope + reinstall + token check (same wording as the
  corrections setup).
- Slack rate tiers: `files.getUploadURLExternal` / `files.completeUploadExternal` are Tier 4 — well
  above rescue-thread volume.

## Testing

- **Unit/integration (`audio_test.go`, DispatchSuite)**:
  - enabled + audio ⇒ one `UploadFileV2Context` with channel `C-TEST`, the thread ts, filename =
    S3 basename, title `"TAC 10 · 14:02:11"`-style, `FileSize == len(audio)`;
  - flag off ⇒ no upload; empty audio ⇒ no upload; oversize ⇒ no upload;
  - upload error ⇒ no panic, no error propagated (process call still returns nil).
- **Call-site ordering**: TAC path — mock `Run` callbacks record order ⇒ post, then upload, then
  `SummarizeRescue`. Dispatch path — upload targets the alert's thread ts and happens after the
  alert post. Re-page — upload into `meta.ThreadTS`.
- `audioTitle` unit test (zero time, local-time formatting).
- Existing tests: pass `nil` audio; test configs leave the flag false ⇒ unaffected.

## Manual verification (cannot be proven in CI)

On the dev stack with a real Slack app (`make push-message`): confirm the WAV reply appears
directly under each transmission, and whether Slack plays WAV inline or only offers download.
If it only downloads, file a follow-up to transcode to MP3 (needs ffmpeg in the image).

## Docs

CLAUDE.md: architecture-table row (audio replies, best-effort, flag, bytes passed from
processRecord); file map entry for `audio.go`; Stack/Slack notes for the `files:write` scope.
