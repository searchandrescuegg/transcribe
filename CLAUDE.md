# CLAUDE.md

Guidance for AI agents and humans onboarding to this repo. Read this before making changes.

The README is the user-facing "how do I run this" doc. This file is the **internal** doc:
load-bearing decisions, invariants you can't violate without breaking things, and the
gotchas surfaced during development.

---

## What this is

Radio-traffic monitoring service for trail rescue. Trunk-Recorder uploads `.wav` files to
S3, the service consumes S3 events from Pulsar, fetches audio, transcribes via an ASR
endpoint, and routes based on the talkgroup encoded in the filename:

- **Fire Dispatch (1399)** → run through an OpenAI-compatible LLM with structured output;
  if the model identifies a trail rescue, the assigned TAC channel is allow-listed and a
  Slack alert posts with operator controls.
- **Allowed TAC (1377/1379/1381/1383/1385/1387/1389/1963/1965/1967)** → transcribe and
  post in the rescue thread; trigger a re-summarization that updates a "Live
  Interpretation" message in place.

When the activation window expires, a Dragonfly-backed sweeper auto-closes the rescue:
parent alert is rewritten in place, "Channel Closed" reply lands, and a Submit Feedback
button opens a Google Form prefilled with incident context.

---

## Architecture decisions

| Decision | Rationale | Where |
|---|---|---|
| Pulsar with explicit Ack-after-process + DLQ | At-least-once with bounded retry; poison messages don't pin a partition | `internal/transcribe/transcribe.go` (handleMessage), `internal/pulsar/client.go` |
| Per-S3-key dedup via Dragonfly `SETNX` | Pulsar redelivers freely; dedup prevents double-Slack-post | `processRecord` in `transcribe.go` |
| Dragonfly (Redis-protocol-compatible) | Uses `SADDEX` (per-member TTL) for `allowed_talkgroups` — Dragonfly-specific, not stock Redis | `internal/dragonfly/dragonfly.go` |
| Durable closure scheduling via ZSET sweeper | Replaces in-process `time.AfterFunc` that lost scheduled messages on restart | `internal/transcribe/sweeper.go` |
| Slack interactivity over **Socket Mode** | No public HTTP endpoint; bot opens outbound WebSocket | `internal/slackctl/controller.go` |
| Live interpretation: per-TGID `SETNX` lock + stale flag | Prevents N concurrent LLM calls when N transmissions arrive simultaneously; bounds to ~2 LLM calls per burst | `internal/transcribe/live_interpretation.go` |
| Confidential call types: AES-256-GCM, env-supplied key | Encrypted file in repo, key in secret manager | `internal/calltypes/calltypes.go` |
| Process-global `time.Local` override at startup | Single point of truth for display TZ; all `.Local()` calls inherit | `cmd/transcribe/main.go` |
| `chat_template_kwargs.enable_thinking=false` per-request + `<think>` stripper | First-party server-side switch for Qwen3-family + defense-in-depth post-hoc strip | `internal/openai/openai.go` |
| Each ML backend implements `DispatchMessageParser`, `RescueSummarizer`, AND `TranscriptCleaner` | One concrete client per backend, three interfaces; `transcribe.MLClient` combines them | `internal/transcribe/transcribe.go` (interface), `internal/openai/openai.go` + `internal/anthropic/anthropic.go` (impls) |
| `SlackPoster` interface with `SendMessageContext` + `UpdateMessageContext` | `*slack.Client` satisfies structurally; tests inject testify mock | `internal/transcribe/transcribe.go` |
| Two selectable ML backends behind one interface: OpenAI-compatible and first-party Anthropic | `ML_BACKEND` chooses at startup; both implement `transcribe.MLClient`. Ollama/vLLM/LiteLLM still work via `OPENAI_BASE_URL` | `cmd/transcribe/main.go`, `internal/openai/`, `internal/anthropic/` |
| Shared prompt + schema contract in `internal/prompts` | Single source of truth so the two backends can't drift on wording or output shape | `internal/prompts/prompts.go`, `internal/prompts/schema.go` |
| Anthropic backend uses native structured outputs (`output_config.format`) with thinking disabled | GA structured-output path; thinking off keeps short extraction/classification calls fast and bounded to `WorkerTimeout` | `internal/anthropic/anthropic.go` |
| Optional Postgres dataset capture: async best-effort decorator + direct writes | Compile a transcription + LLM-I/O corpus for prompt refinement without ever blocking the pipeline (drops on full buffer, never errors upward) | `internal/dataset/`, `internal/transcribe/transcribe.go` (processRecord), `cmd/transcribe/main.go` |
| SAR-notified green-check badge, latched via the `summary_data` false→true transition | Surfaces "Search & Rescue notified" on the parent alert + live interpretation; fires exactly one extra `chat.update` per rescue and adds **no** new sidecar key (transition detected by reading `summary_data` before overwrite; expiry for the mid-rescue re-render read from `active_tacs` via `ZScore`) | `internal/prompts` (rule #10), `live_interpretation.go` (`badgeParentAlertSAR`), `slack.go` (`buildSARNotifiedBlock`), `sweeper.go` (`summarySARNotified`, closed-alert badge) |
| Additive live-interpretation summary: prev summary fed back as input | Stops key-event churn — the model EXTENDS its prior summary (preserve established KeyEvents, append new) instead of re-deriving from scratch each transmission. Prev summary read from the existing `summary_data` (no new key); missing/garbled prior degrades to the old full-rewrite behavior | `internal/prompts` (summary rule #12 + `renderPreviousSummary`), `ml.RescueSummaryInput.PreviousSummary`, `live_interpretation.go` (`runOneSummaryPass` reads `readSummaryData`) |
| Per-transmission LLM cleanup of TAC traffic | Raw ASR TAC transmissions were posted verbatim; a dedicated (cheap-model) cleanup call fixes ASR errors, place names (gazetteer), and unit callsigns before the thread reply AND the summary. Best-effort with raw fallback; kill-switch `TAC_CLEANUP_ENABLED` | `internal/ml` (`TranscriptCleaner`), `internal/prompts` (`TACCleanupSystemPrompt`), both backends' `CleanTACTranscript`, `process.go` (`maybeCleanTranscript`) |
| CAD (PulsePoint) unit enrichment behind an optional resolver | Feeds the units actually assigned to the call into cleanup + summary so garbled callsigns snap to real units. Fuzzy correlation (location + call-type + recency) with agency-roster fallback; entirely best-effort and flag-gated (`PULPO_ENABLED`). Cached per-rescue in `pulpo_units:<TGID>` | `internal/pulsepoint/` (Resolver, `selectUnitContext`, `UnitContext.PromptBlock`), `transcribe.UnitResolver`, `unit_context.go` (`unitContextFor`) |
| Same-incident dispatch dedup keyed on active `tac_meta:<TGID>` | A TAC is one incident at a time, so a 2nd tone-out naming an already-active TAC is an additional unit, not a new rescue. `processDispatchCall` checks `readClosureMeta` before posting; if active it refreshes the window + posts a thread reply instead of a 2nd alert — which would overwrite `tg:<TGID>` and orphan the original thread. (Residual: near-simultaneous first tone-outs can still double-post; the "different times" case is covered.) | `process.go` (`handleAdditionalDispatch`), `slack.go` (`BuildAdditionalDispatchBlocks`) |
| Delete button targets the clicked message, not `tac_meta.MessageTS` | Orphaned duplicates share a TGID with the live alert, so a TGID-keyed delete would nuke the live one. `rescue_delete` deletes `payload.Container.MessageTs` and only tears down state when that ts IS the live alert. | `slackctl/delete.go` |
| Trail-rescue detection has a transcription safety net, not just the LLM call_type | The LLM classifier can mislabel a trail rescue as another rescue subtype (prod 2026-07-15: "Rescue Trail Tac 2" → "Rescue - General" → **no alert**). A missed trail rescue is the worst failure mode, so `selectTrailRescueMessage` salvages any dispatch whose RAW transcription says "rescue trail"/"trail rescue" (adjacent phrase — narrow to avoid false positives), taking the TAC from the parsed message or a regex on the text. Prompt also nudges toward the trail-rescue value. | `rules.go` (`transcriptionSignalsTrailRescue`, `tacChannelFromText`), `process.go` (`selectTrailRescueMessage`), `prompts.go` |

---

## Critical invariants — DO NOT VIOLATE

These are non-obvious orderings or constraints that are load-bearing across multiple files.
Each was discovered (and fixed) during development; comments in code reference these names.

1. **Dedup-set MUST happen AFTER `IsObjectAllowed=true`** — otherwise rejected messages
   burn a dedup slot and a redelivered/republished copy is silently skipped even after
   the TAC makes it onto the allow-list. (`processRecord`)

2. **`dispatch_in_flight` marker MUST be set AFTER the dedup check passes** — otherwise a
   stale dedup key (operator re-runs the synthetic trigger) causes the marker to be
   stamped with no actual dispatch processing behind it; every TAC for the next
   `WorkerTimeout` window then nack-loops chasing an allow-list write that won't happen.
   (`processRecord`)

3. **`dispatch_in_flight` marker MUST be cleared on processRecord exit** — a deferred
   `Del` runs immediately after the `Set`, so success, error, non-rescue, and panic paths
   all reach it. Without the defer, a 1399 dispatch that the LLM classifies as anything
   other than a trail rescue (Smoke - Burn Complaint, Aid Emergency, etc.) leaves the
   marker set for the full `WorkerTimeout` window — every concurrent TAC transmission
   nacks-for-retry against a phantom dispatch and eventually DLQs. The TTL is a safety
   net; the deferred `Del` is authoritative. (`processRecord`)

4. **Sweeper sidecar cleanup MUST run AFTER `postChannelClosed`, not before** — otherwise
   `summary_data:<TGID>` is deleted before `buildFeedbackURL` reads it, and the feedback
   form silently loses the headline + situation_summary prefill. The cleanup uses an
   inline `cleanup()` closure called explicitly on every exit path. (`sweepOnce` in
   `sweeper.go`)

5. **`IsObjectAllowed` returns the parsed key on rejection** — the nack-recovery error
   message in `processRecord` reads `parsedKey.dk.Talkgroup`. If `IsObjectAllowed` returns
   nil parsed-key, this nil-derefs. (`rules.go`)

6. **Cancel / Switch / sweeper close MUST clean up ALL sidecars**: `tac_meta:<TGID>`,
   `tac_transcripts:<TGID>`, `summary_ts:<TGID>`, `summary_lock:<TGID>`,
   `summary_stale:<TGID>`, `summary_data:<TGID>`, `pulpo_units:<TGID>`, `tg:<TGID>`, plus
   SREM from `allowed_talkgroups` and ZREM from `active_tacs`. Missing one = a
   closed-then-reopened rescue inherits stale state from the prior incident. (`sweeper.go`,
   `slackctl/cancel.go`, `slackctl/switch_tac.go`)

7. **`WorkerTimeout` must cover EVERY serial LLM call in a worker, not just one** — the worker
   context wraps the whole record. A TAC transmission now runs the cleanup call THEN the summary
   call sequentially, so the budget must exceed `TACCleanupTimeout + summary round-trip` (plus S3
   + ASR), not merely one LLM timeout. `TACCleanupTimeout` (default 20s) sub-bounds the cleanup so
   it can't starve the thread reply + summary; keep `WorkerTimeout` comfortably above the sum.
   Currently 180s worker vs 120s OpenAI (or 30s Anthropic) per call. (`internal/config/config.go`,
   `process.go` `maybeCleanTranscript`)

8. **`time.Local` override happens BEFORE any time-formatting code runs** — set in
   `main.go` immediately after config + slog are wired. Tests that depend on display
   format set `time.Local` themselves (see `slack_test.go`). The binary embeds Go's
   tzdata via `_ "time/tzdata"` import so distroless-static can resolve any IANA zone.

9. **URL buttons in Slack still fire `block_actions` events** — they need a no-op case in
   the controller dispatch (`ActionIDFeedbackForm`) or the dispatch logs a noisy
   `unknown action_id` WARN on every click. (`internal/slackctl/controller.go`)

10. **The ASR response field is `text`, not `transcription`** — the cluster ASR returns
    `{"text": "..."}` while the old mock returned `{"transcription": "..."}`. The struct in
    `internal/asr/client.go` uses `json:"text"`. If you ever swap ASR backends, this is
    the first thing to check.

11. **`SLACK_ALLOWED_USER_IDS=*` short-circuits the leadership gate** — the empty-list
    behavior is "deny all" (safe default); `*` is "allow all" (intentional choice, logged
    at WARN). Don't accidentally make empty-list mean "allow all". (`slackctl/controller.go`)

---

## Dragonfly key conventions

These keys are written and read across multiple files. **A schema change here means
updating the constants in BOTH `internal/transcribe/` and `internal/slackctl/cancel.go`**
(the controller mirrors the constants rather than importing them, so a schema change is
visible at both layers).

| Key | Type | Owner | TTL | Purpose |
|---|---|---|---|---|
| `allowed_talkgroups` | SET (with SADDEX per-member TTL) | `processDispatchCall` writes; `IsObjectAllowed` reads; Cancel/Switch SREM | per-member: `TacticalChannelActivationDuration` | Allow-list of currently-active TACs |
| `tg:<TGID>` | STRING | `processDispatchCall` writes; `processNonDispatchCall` reads | `TacticalChannelActivationDuration` | Per-talkgroup → thread_ts routing for follow-up TAC traffic |
| `active_tacs` | ZSET (member=TGID, score=unix expiry) | `ScheduleTACClosure` writes; sweeper reads + ZRems | none (cleaned by sweeper) | Pending channel-closed notifications |
| `tac_meta:<TGID>` | STRING (JSON `ClosureMeta`) | `ScheduleTACClosure` writes; sweeper + Cancel/Switch + feedback URL build read | 24h safety net | Closure metadata: TAC channel, thread_ts, dispatch transcription, message_ts |
| `dedup:<S3-key>` | STRING (`"1"`) | `processRecord` SETNX | `DedupTTL` (default 1h) | Per-S3-object idempotency |
| `dispatch_in_flight` | STRING (`"1"`) | `processRecord` (only for 1399 events post-dedup) | `WorkerTimeout` | Marker enabling nack-recovery for racing TAC traffic |
| `tac_transcripts:<TGID>` | LIST (JSON entries) | `updateLiveInterpretation` RPushes; reads via LRange | 2 × `TacticalChannelActivationDuration` | Per-TAC ordered transcript history for cumulative summarization |
| `summary_ts:<TGID>` | STRING | `publishLiveInterpretation` writes on first post | 2 × `TacticalChannelActivationDuration` | Cached message_ts so subsequent updates `chat.update` instead of re-posting |
| `summary_data:<TGID>` | STRING (JSON `RescueSummary`) | `publishLiveInterpretation` writes after every summarize | 2 × `TacticalChannelActivationDuration` | Latest structured summary; read by sweeper for feedback URL prefill |
| `summary_lock:<TGID>` | STRING (`"1"`) | `updateLiveInterpretation` SETNX before LLM call | 150s | Per-TGID exclusion: bounds concurrent LLM calls to ~2 per burst |
| `summary_stale:<TGID>` | STRING (`"1"`) | Set by lock losers; cleared by lock holder | 60s | "New transmission arrived during your work — rerun summary" |
| `pulpo_units:<TGID>` | STRING (rendered unit block, or `\x00none` sentinel) | `unitContextFor` / `resolveAndCacheUnitContext` write; cleanup DELs | `PulpoRefreshInterval` (default 45s) | Cached CAD unit-context block for cleanup + summary; short TTL so the roster self-refreshes as units are added |

---

## Slack interactivity model

Five buttons + one URL button on every rescue alert (when `SLACK_APP_TOKEN` is configured):

| Action ID | Type | Authorized? | Effect |
|---|---|---|---|
| `rescue_cancel` | Button (danger) + confirm | Yes (allowlist) | SREM allow-list, DEL all sidecars, post cancellation reply, chat.update alert to "Cancelled" — wipes summary context |
| `rescue_close` | Button + confirm | Yes (allowlist) | Early end-of-rescue routed through the **same path as auto-expiry**: SREM allow-list + DEL routing inline (so TAC traffic stops immediately), then ZADD `active_tacs` with score=now-1 so the sweeper claims it on its next tick (~5s) and runs `postChannelClosed` + `updateAlertForClosure` (preserving `summary_data` for the feedback URL prefill) + sidecar cleanup |
| `rescue_extend` | Button + confirm | Yes (allowlist) | Refresh all per-TGID TTLs to a fresh activation window |
| `rescue_switch_tac` | Static-select + confirm | Yes (allowlist) | Migrate state from old TGID to new TGID; preserve thread_ts |
| `rescue_delete` | Button (danger) + confirm | Yes (allowlist) | chat.delete **the specific clicked message** (`payload.Container.MessageTs`). Smart: if that ts == `tac_meta.MessageTS` it's the live alert → tear the incident down via `CancelTAC` (no tombstone) then delete; otherwise it's an orphan → delete the message only, live incident untouched. (`slackctl/delete.go`) |
| `feedback_form` | URL button (closed alert only) | n/a | Opens Google Form client-side; controller no-ops the resulting `block_actions` event |

Authorization: `SLACK_ALLOWED_USER_IDS` (comma-separated user IDs). Empty = deny all.
Contains `*` = allow all (logged at WARN). The unauthorized ephemeral message
deliberately doesn't say "you're not on the list" — that leaks the existence of an
allowlist. (`controller.go:respondNotAuthorized`)

block_id encoding: the actions block uses `block_id="rescue_actions:<TGID>"` so the
switch-TAC handler can recover the source TGID without a Dragonfly lookup. Cancel/Extend
read TGID from the button's `value` field instead.

---

## Stack

| Tool | Purpose | Notes |
|---|---|---|
| Go 1.24+ | Service language | |
| Apache Pulsar 4.0.2 | Message queue | Shared-subscription consumer with DLQ policy |
| Dragonfly | Cache + scheduler | Redis-protocol; uses `SADDEX` (Dragonfly-specific) |
| s3-ninja (local) / VersityGW (prod) | S3-compatible storage | Local: port 9000 in container, 9444 on host |
| Mockserver (local) / cluster Whisper/Parakeet (prod) | ASR | Real returns `{"text": ..., "no_speech_detected": ...}` |
| OpenAI-compatible chat completions | Dispatch parsing + rescue summarization | llama.cpp, Ollama, vLLM, OpenAI all work via `OPENAI_BASE_URL` |
| `slack-go/slack` (+ `socketmode` submodule) | Slack client | Uses `chat_template_kwargs` field |
| `sashabaranov/go-openai` | OpenAI client | Has `ChatTemplateKwargs` field for vLLM/llama.cpp passthrough; also provides the shared `jsonschema` generator |
| `anthropics/anthropic-sdk-go` | Anthropic client | Used when `ML_BACKEND=anthropic`; native structured outputs via `output_config.format` |
| `caarlos0/env/v11` | Config from env | All config in `internal/config/config.go` |
| `testify` (suite, mock, assert, require) | Testing | |
| `testcontainers-go` | Integration tests | Spins up Dragonfly + Pulsar containers |
| `apache/pulsar-client-go` | Pulsar client | |
| `redis/go-redis/v9` | Redis client (for Dragonfly) | |
| Postgres 17 | Dataset store (optional) | Enabled via `DATASET_ENABLED`; local via docker-compose, self-hosted in prod |
| `jackc/pgx/v5` | Postgres driver | Used through `database/sql` (`stdlib`) for dataset writes |
| `pressly/goose/v3` | DB migrations | Embedded SQL migrations applied at startup |

---

## Common gotchas

1. **Don't run `make push-message` without flushing Dragonfly** if the previous run's
   dedup keys are still alive (1h default). Either `docker compose exec dragonfly
   redis-cli FLUSHDB` or set `DEDUP_TTL` low for testing.

2. **`TACTICAL_CHANNEL_ACTIVATION_DURATION=2m`** (or any short value) is for testing the
   sweeper close path. Restore to `30m` (or your operating window) for production.

3. **Dragonfly auto-sizes memory aggressively.** Test containers MUST pass
   `--proactor_threads=2 --maxmemory=512mb` or they refuse to start when the dev stack is
   running. Both `internal/transcribe/integration_test.go` and
   `internal/slackctl/controller_test.go` already do this.

4. **s3-ninja port mismatch**: container listens on 9000; host port maps to 9444. From
   inside docker network, hit `http://s3ninja:9000`. From the host browser, `:9444`.
   Wrong port → `connection refused` errors in main's S3 client.

5. **`make push-message ARGS='-delay 30s'` requires the Makefile fix** — the previous
   version inserted a `--` separator that Go's `flag.Parse()` interpreted as
   end-of-flags, silently dropping `-delay`. Current Makefile passes `$(ARGS)` directly.

6. **WAV fixtures must be real RIFF PCM**, not M4A bytes under a `.wav` filename. The
   real cluster ASR rejects mismatched format with HTTP 400. Use `ffmpeg -i in.m4a -ac 1
   -ar 16000 -c:a pcm_s16le out.wav` to transcode.

7. **`docker compose up -d` does NOT pick up `.env` changes for already-running services.**
   Use `docker compose up -d --force-recreate main` after editing `.env`.

8. **`OPENAI_API_KEY` must be set** when pointing at the cluster's llama-cpp (deployment
   uses `--api-key-file`). Decrypt from the SOPS secret in `lfprocks/k8s` — the README
   has the command.

9. **`chat_template_kwargs: {"enable_thinking": false}`** is sent on every request.
   Templates that don't recognize the kwarg (Gemma 3/4, Mistral) silently ignore it.
   The `<think>...</think>` stripper in `openai.go` is defense-in-depth for models that
   leak thinking despite the kwarg.

10. **The `feedback_form` action_id needs a no-op case** in `controller.go`'s dispatch
    switch. URL buttons fire `block_actions` events even though Slack also opens the URL
    client-side — without the no-op case, every click logs `WARN unknown action_id`.

11. **The dispatch parser's structured output uses an `enum` constraint on `call_type`**
    when `CALL_TYPES_PATH` is configured (`Strict: true`). The model can ONLY emit
    values from the encrypted list (plus `Unknown`). With no constraint, the prompt
    falls back to in-line examples.

---

## File map

| Path | Purpose |
|---|---|
| `cmd/transcribe/main.go` | Service entry point; wires config, ootel, logging, TZ override, all clients, worker pool, sweeper, Slack controller |
| `cmd/push-message/main.go` | Synthetic trigger — replays fixture .wav files as Pulsar S3 events |
| `cmd/encrypt-calltypes/main.go` | Operator CLI: `generate-key`, `encrypt`, `decrypt` for the confidential call-types file |
| `cmd/test-transcription/main.go` | Iterate on the dispatch-parser prompt against arbitrary transcript files |
| `cmd/test-summary/main.go` | Iterate on the rescue-summarizer prompt against arbitrary `{dispatch, tac[]}` JSON |
| `internal/transcribe/transcribe.go` | `Work`, `handleMessage`, `processRecord` — top-level message lifecycle |
| `internal/transcribe/process.go` | `processDispatchCall`, `processNonDispatchCall` |
| `internal/transcribe/sweeper.go` | `Sweep`, `sweepOnce`, `postChannelClosed`, `updateAlertForClosure` — durable closure scheduling |
| `internal/transcribe/live_interpretation.go` | `updateLiveInterpretation` + per-TGID lock pattern; additive summary (prev summary fed back) |
| `internal/transcribe/unit_context.go` | `unitContextFor` / `resolveAndCacheUnitContext` — per-rescue CAD unit-context cache (best-effort, nil-resolver safe) |
| `internal/transcribe/feedback.go` | `buildFeedbackURL` — Google Form URL with prefill |
| `internal/transcribe/slack.go` | All Slack block builders (alert, live interpretation, channel closed, thread comm, action block, feedback button) |
| `internal/transcribe/slack_send.go` | `sendSlackWithRetry` (rate-limit-aware single retry) |
| `internal/transcribe/rules.go` | `IsObjectAllowed`, `CallIsTrailRescue` |
| `internal/transcribe/talkgroups.go` | NORCOM talkgroup table — single source of truth; short-code map derived in `init()` |
| `internal/transcribe/parse.go` | `parseKey` — Trunk-Recorder filename parser |
| `internal/slackctl/controller.go` | Socket Mode event loop, dispatch, authorization |
| `internal/slackctl/cancel.go` | `CancelTAC` state mutations + `handleCancel` Slack-side wiring |
| `internal/slackctl/extend.go` | `ExtendTAC` + `handleExtend` |
| `internal/slackctl/switch_tac.go` | `SwitchTAC` + `handleSwitchTAC`; `parseOldTGIDFromBlockID` |
| `internal/prompts/prompts.go` | Shared prompt text + system/user prompt builders for both ML backends |
| `internal/prompts/schema.go` | Shared response-schema builders (`DispatchSchema` with enum injection, `RescueSummarySchema`) |
| `internal/openai/openai.go` | `OpenAIClient` (OpenAI-compatible) implementing both `DispatchMessageParser` and `RescueSummarizer`; delegates prompts/schema to `internal/prompts` |
| `internal/anthropic/anthropic.go` | `Client` (first-party Anthropic) implementing all three ML interfaces via native structured outputs; per-task models (dispatch / summary / cleanup, each independently configurable) |
| `internal/pulsepoint/` | Optional CAD (PulsePoint) unit enrichment: `Resolver` over the `pulpo` client, fuzzy incident match (`selectUnitContext`) with agency-roster fallback, `UnitContext.PromptBlock` |
| `internal/dataset/dataset.go` | Dataset records, `Recorder` interface, `RecordingMLClient` decorator, request-context source correlation |
| `internal/dataset/store.go` | Postgres-backed `Recorder`: embedded goose migrations + async best-effort writer |
| `internal/dataset/migrations/*.sql` | goose migrations for the `transcriptions` + `llm_interactions` tables |
| `internal/calltypes/calltypes.go` | AES-256-GCM encrypt/decrypt + parser for the call-types file |
| `internal/dragonfly/dragonfly.go` | Dragonfly client wrapper (per-method timeouts) |
| `internal/pulsar/client.go` | Pulsar consumer wrapper with DLQ policy |
| `internal/asr/client.go` | ASR HTTP client (multipart upload) |
| `internal/ml/interfaces.go` | All ML data types + interfaces: `DispatchMessageParser`, `RescueSummarizer`, `TranscriptCleaner`; `DispatchMessages`, `RescueSummary`, `RescueSummaryInput` (with `PreviousSummary`/`UnitContext`), `TACCleanupInput`/`TACCleanupResult`, etc. |
| `internal/config/config.go` | Single source of truth for env-var config |
| `slack/manifest.yaml` | Slack app manifest — paste at api.slack.com/apps |
| `config/call_types.example.txt` | Sample plaintext for the encrypted call-types file |
| `data/rescue.example.json` | Sample fixture for `cmd/test-summary` |
| `docker/s3ninja/audio/*.wav` | Fixture audio files (Trunk-Recorder filename convention; real WAV bytes) |

---

## Test conventions

- **Unit tests** in `*_test.go` next to the file under test. Pure logic only. Run with
  `go test ./<pkg>/...`.
- **Integration tests** in `integration_test.go` (transcribe pkg) and `controller_test.go`
  (slackctl pkg). Use `testify/suite` for shared container lifecycle.
- **Containers**: Dragonfly + Pulsar via `testcontainers-go`. Suite-scoped (started in
  `SetupSuite`, terminated in `TearDownSuite`). State reset per-test via `SetupTest` →
  `FlushDB`.
- **Mocks**: `mockSlackPoster` and `mockMLClient` in `internal/transcribe/integration_test.go`.
  testify `.Run()` callbacks are useful for asserting mid-call state (e.g. "is summary_data
  still alive when chat.update fires?").
- **What to test where**: state-mutation logic (Dragonfly side) → integration tests.
  Pure helpers (URL building, regex stripping) → plain unit tests. Slack message rendering
  → block-builder unit tests.
- **Full sweep**: `go test -count=1 -timeout 5m ./...` (~25s with warm container images).

---

## Run / iterate quick reference

```bash
# One-time
cp .env.example .env && $EDITOR .env

# Bring up the stack
docker compose up -d
docker compose logs -f main

# Synthetic trigger (default 3s spacing — too fast for live interpretation to keep up)
make push-message

# Realistic spacing — recommended for testing the full live-interpretation flow
make push-message ARGS='-delay 30s'

# Single fixture
make push-message ARGS='-file 1399-1777832036_852162500.0-call_001.wav'

# Reset Dragonfly between runs
docker compose exec dragonfly redis-cli FLUSHDB

# Recreate main after .env change
docker compose up -d --force-recreate main

# Iterate on the rescue-summary prompt
OPENAI_API_KEY=... OPENAI_BASE_URL=... OPENAI_MODEL_NAME=... \
  go run ./cmd/test-summary -input data/rescue.example.json

# Iterate on the dispatch-parser prompt
OPENAI_API_KEY=... OPENAI_BASE_URL=... OPENAI_MODEL_NAME=... \
TRANSCRIPTION_FILE=data/transcription.txt \
  go run ./cmd/test-transcription
```

---

## Prompt iteration

The two LLM-driven features have their prompts as constants in
`internal/prompts/prompts.go` (shared by both the OpenAI and Anthropic backends):

- **Dispatch parsing**: `DispatchSystemPrompt(allowedCallTypes)` returns `defaultSystemPrompt`
  (when `allowedCallTypes` is empty) or `constrainedSystemPromptHead` + per-call enum +
  `constrainedSystemPromptTail` (when configured). Edits affect every dispatch event.
- **Rescue summarization**: `RescueSummarySystemPrompt`. Edits affect every TAC
  transmission.

After editing, iterate via `cmd/test-transcription` or `cmd/test-summary` against fixture
inputs. Once the output looks right, the live pipeline picks up the change on the next
binary build (no env or schema migration needed — the prompt constants are baked in).

If you change the `RescueSummary` struct shape (rename a field, add a new one), the
structured-output JSON schema is generated from the struct via
`jsonschema.GenerateSchemaForType`, so the model's response shape changes automatically —
but BLOCK BUILDER and FEEDBACK URL code that reads those fields needs hand-updating.
Search for the field name across `internal/transcribe/slack.go` and
`internal/transcribe/feedback.go`.
