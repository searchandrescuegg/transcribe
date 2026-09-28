# Alert Brief Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Show a page-out-style brief ("Mailbox Peak · 54F · ankle injury") under the header of the parent rescue alert, derived from the Live Interpretation summary and re-rendered only when it changes.

**Architecture:** Three new structured-output fields on `ml.RescueSummary` (shared schema → both backends), a pure `FormatBrief` renderer, and a bold section block in `BuildRescueTrailBlocks`. `rerenderParentAlert` becomes the single parent-alert render path and derives both the SAR badge and the brief from `summary_data`, so every caller (summary pass, dispatch correction) carries every decoration. `publishLiveInterpretation` detects brief changes against the previous `summary_data` (no new keys) and does at most one parent `chat.update` per pass.

**Tech Stack:** Go 1.25, `slack-go/slack` v0.17.1, `sashabaranov/go-openai` jsonschema generator, Dragonfly (`redis/go-redis/v9`), `testify`, `testcontainers-go`.

**Spec:** `docs/superpowers/specs/2026-09-27-alert-brief-design.md`

## Global Constraints

- Branch `feat/alert-brief` (stacked on `feat/human-corrections`); do not switch branches.
- Commits are GPG-signed automatically (`commit.gpgsign=true`) — never pass `--no-gpg-sign`, never use `git filter-branch`.
- Commit subjects: conventional, lowercase first word, ≤ 100 chars (CI runs commitlint `config-conventional`). Every commit message ends with:
  ```
  Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01UGf82rADsPjLxqirG637Y2
  ```
- Brief slots: `brief_location`, `brief_subject`, `brief_condition`; each 1–3 words, empty when unknown; joined with ` · `; each truncated to 40 runes with `…`.
- Brief line is a bold mrkdwn section directly under the header, above the SAR badge.
- Empty brief ⇒ parent-alert blocks byte-identical to today.
- The brief never comes from the dispatch parser; no new Dragonfly keys; at most one parent `chat.update` per summary pass, only when the brief changed or SAR flipped false→true.
- Brief location must never be inferred or invented (prompt rule 17).
- Do not commit the untracked `grafana/` directory or the `./transcribe` binary.

## Review Focus

1. A dispatch correction made after a brief exists — the correction's re-render must keep the brief (it previously passed only SAR). (Task 3)
2. Model output containing `*`, `_`, `<`, `&`, or newlines in a slot — the bold wrapper must not break. (Task 2)
3. Two consecutive summary passes with the same brief — no parent `chat.update` storm on every transmission. (Task 3)
4. SAR flip and brief change in the same pass — exactly one parent update carrying both. (Task 3)
5. A rescue that closes after a brief was set — the closed alert keeps it. (Task 3)

---

### Task 1: Brief fields in the summary schema and prompt

**Files:**
- Modify: `internal/ml/interfaces.go` (`RescueSummary`, after `Outcome`)
- Modify: `internal/prompts/prompts.go` (rule 16 at ~line 106 is the last rule; `renderPreviousSummary` ~line 188-196)
- Test: `internal/prompts/prompts_test.go`, `internal/prompts/schema_test.go` if present (else add to prompts_test.go)

**Interfaces:**
- Produces: `ml.RescueSummary.BriefLocation`, `.BriefSubject`, `.BriefCondition` (strings; JSON `brief_location`, `brief_subject`, `brief_condition`).

- [ ] **Step 1: Write the failing tests** (append to `internal/prompts/prompts_test.go`)

```go
func TestRescueSummaryPrompt_BriefRule(t *testing.T) {
	assert.Contains(t, RescueSummarySystemPrompt, "17. BRIEF")
	assert.Contains(t, RescueSummarySystemPrompt, "never infer or invent")
	assert.Contains(t, RescueSummarySystemPrompt, "BriefLocation")
}

func TestRenderPreviousSummary_IncludesBrief(t *testing.T) {
	out := renderPreviousSummary(&ml.RescueSummary{BriefLocation: "Mailbox Peak", BriefCondition: "ankle injury"})
	assert.Contains(t, out, "Brief: Mailbox Peak · - · ankle injury")
}

// The strict structured-output schema must carry (and require) the new fields.
func TestRescueSummarySchema_HasBriefFields(t *testing.T) {
	s, err := RescueSummarySchema()
	require.NoError(t, err)
	for _, f := range []string{"brief_location", "brief_subject", "brief_condition"} {
		_, ok := s.Properties[f]
		assert.True(t, ok, "schema property %s", f)
		assert.Contains(t, s.Required, f)
	}
}
```

(Add `"github.com/stretchr/testify/require"` to the imports if absent.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/prompts/...`
Expected: FAIL — unknown fields / missing rule text.

- [ ] **Step 3: Implement**

In `internal/ml/interfaces.go` `RescueSummary`, after `Outcome`:

```go
	// Brief* are the page-out-style slots rendered as one bold line on the parent alert
	// ("Mailbox Peak · 54F · ankle injury"). Each is a few words; empty when not stated.
	BriefLocation  string `json:"brief_location"`
	BriefSubject   string `json:"brief_subject"`
	BriefCondition string `json:"brief_condition"`
```

In `rescueSummarySystemPromptBase`, append after rule 16 (keep the closing backtick after rule 17):

```
17. BRIEF: Fill BriefLocation, BriefSubject, and BriefCondition as a dispatcher page-out would read — terse fragments, not sentences. BriefLocation: the best-known place in 1–3 words (trail, peak, trailhead, or road — e.g. "Mailbox Peak", "Rattlesnake Ledge"); use ONLY a place actually stated in the dispatch, transmissions, or operator corrections — never infer or invent one. BriefSubject: who needs help in 1–3 words — age+sex shorthand when known ("54F", "30M"), otherwise a count or role ("2 hikers", "hiker", "climber"). BriefCondition: the medical problem or situation in 1–3 words ("ankle injury", "cardiac", "lost, uninjured"). Leave any slot empty when it is not stated — never guess. Operator corrections override (rule 15). Keep the brief stable between updates; change a slot only when new information warrants it.
```

In `renderPreviousSummary`, after the `Outcome` line:

```go
	fmt.Fprintf(&b, "Brief: %s · %s · %s\n", emptyAsDash(s.BriefLocation), emptyAsDash(s.BriefSubject), emptyAsDash(s.BriefCondition))
```

- [ ] **Step 4: Run tests to verify pass**

Run: `go build ./... && go test ./internal/prompts/... ./internal/openai/... ./internal/anthropic/... ./internal/dataset/...`
Expected: PASS (backends and the dataset decorator must still pass with the widened struct).

- [ ] **Step 5: Commit**

```bash
git add internal/ml/interfaces.go internal/prompts/
git commit -F - <<'EOF'
feat(prompts): add page-out brief slots to the rescue summary

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01UGf82rADsPjLxqirG637Y2
EOF
```

---

### Task 2: FormatBrief and the alert brief block

**Files:**
- Modify: `internal/transcribe/slack.go` (`RescueTrailBlocksInput`, `BuildRescueTrailBlocks` — SAR insertion is `blocks = append(blocks[:1:1], ...)` after the correction-label insertion)
- Create: `internal/transcribe/slack_brief_test.go`

**Interfaces:**
- Consumes: Task 1 fields.
- Produces: `func FormatBrief(s *ml.RescueSummary) string`; `RescueTrailBlocksInput.Brief string`; `func buildBriefBlock(brief string) slack.Block`.

- [ ] **Step 1: Write the failing tests** (create `internal/transcribe/slack_brief_test.go`)

```go
package transcribe

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/searchandrescuegg/transcribe/internal/ml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFormatBrief(t *testing.T) {
	cases := []struct {
		name string
		in   *ml.RescueSummary
		want string
	}{
		{"nil", nil, ""},
		{"all empty", &ml.RescueSummary{}, ""},
		{"all slots", &ml.RescueSummary{BriefLocation: "Mailbox Peak", BriefSubject: "54F", BriefCondition: "ankle injury"}, "Mailbox Peak · 54F · ankle injury"},
		{"empty middle dropped", &ml.RescueSummary{BriefLocation: "Tiger Mtn", BriefCondition: "cardiac"}, "Tiger Mtn · cardiac"},
		{"whitespace collapsed", &ml.RescueSummary{BriefLocation: "  Mailbox\n  Peak ", BriefSubject: "   "}, "Mailbox Peak"},
		{"mrkdwn escaped and bold-breakers stripped", &ml.RescueSummary{BriefLocation: "A & B <x>", BriefCondition: "*bad*_leg_"}, "A &amp; B &lt;x&gt; · badleg"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { assert.Equal(t, tc.want, FormatBrief(tc.in)) })
	}

	long := FormatBrief(&ml.RescueSummary{BriefLocation: strings.Repeat("x", 60)})
	assert.Equal(t, strings.Repeat("x", 40)+"…", long, "each slot truncated to 40 runes")
}

func TestRescueTrailBlocks_Brief(t *testing.T) {
	base := func() *RescueTrailBlocksInput {
		return &RescueTrailBlocksInput{TACChannel: "TAC10", TranscriptionText: "t", ExpiresAt: time.Unix(1_700_000_000, 0), DispatchTGID: "1399", TACTalkgroupTGID: "1967"}
	}
	plain, err := json.Marshal(BuildRescueTrailBlocks(base()))
	require.NoError(t, err)
	withEmpty := base()
	withEmpty.Brief = ""
	same, err := json.Marshal(BuildRescueTrailBlocks(withEmpty))
	require.NoError(t, err)
	assert.Equal(t, string(plain), string(same), "empty brief must be byte-identical")

	all := base()
	all.Brief = "Mailbox Peak · 54F · ankle injury"
	all.SARNotified = true
	all.Correction = &TranscriptCorrection{By: "U1", At: time.Now(), Original: "o"}
	blocks := BuildRescueTrailBlocks(all)
	j := func(i int) string { b, _ := json.Marshal(blocks[i]); return string(b) }
	assert.Contains(t, j(1), "*Mailbox Peak · 54F · ankle injury*", "brief directly under header")
	assert.Contains(t, j(2), "Search \\u0026 Rescue notified", "SAR badge after brief")
	full, _ := json.Marshal(blocks)
	assert.Contains(t, string(full), "Corrected by")
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/transcribe/ -run 'TestFormatBrief|TestRescueTrailBlocks_Brief'`
Expected: compile FAIL — `undefined: FormatBrief`, unknown field `Brief`.

(If the SAR badge JSON encodes `&` differently, adjust only the `j(2)` assertion to match `buildSARNotifiedBlock`'s actual text — the point is ordering.)

- [ ] **Step 3: Implement** (in `internal/transcribe/slack.go`)

Add to `RescueTrailBlocksInput`:

```go
	// Brief is the page-out-style line (FormatBrief output) rendered bold directly under the
	// header. Empty keeps the alert byte-identical to the pre-brief layout.
	Brief string
```

In `BuildRescueTrailBlocks`, immediately AFTER the SAR-badge insertion block (so the brief lands at index 1, above the badge):

```go
	if rtbi.Brief != "" {
		blocks = append(blocks[:1:1], append([]slack.Block{buildBriefBlock(rtbi.Brief)}, blocks[1:]...)...)
	}
```

Append:

```go
// maxBriefSlotRunes bounds each brief slot so a verbose model can't turn the one-line brief
// into a paragraph.
const maxBriefSlotRunes = 40

// FormatBrief renders the summary's page-out slots as "location · subject · condition",
// dropping empty slots. Each slot is whitespace-collapsed, truncated, stripped of the bold
// delimiters (* and _) that would break the wrapper, and mrkdwn-escaped. "" when nothing is set.
func FormatBrief(s *ml.RescueSummary) string {
	if s == nil {
		return ""
	}
	var parts []string
	for _, slot := range []string{s.BriefLocation, s.BriefSubject, s.BriefCondition} {
		slot = strings.Join(strings.Fields(slot), " ")
		slot = strings.NewReplacer("*", "", "_", "").Replace(slot)
		if slot == "" {
			continue
		}
		if r := []rune(slot); len(r) > maxBriefSlotRunes {
			slot = string(r[:maxBriefSlotRunes]) + "…"
		}
		parts = append(parts, strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(slot))
	}
	return strings.Join(parts, " · ")
}

func buildBriefBlock(brief string) slack.Block {
	return slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, "*"+brief+"*", false, false), nil, nil)
}
```

- [ ] **Step 4: Run tests to verify pass**

Run: `go test -count=1 ./internal/transcribe/...` (needs Docker)
Expected: PASS, including existing exact-JSON tests in `slack_test.go` / `slack_sar_test.go`.

- [ ] **Step 5: Commit**

```bash
git add internal/transcribe/slack.go internal/transcribe/slack_brief_test.go
git commit -F - <<'EOF'
feat(transcribe): render the page-out brief under the alert header

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01UGf82rADsPjLxqirG637Y2
EOF
```

---

### Task 3: Parent-alert re-render derives SAR + brief; change-only updates; closed alert

**Files:**
- Modify: `internal/transcribe/corrections.go` (`rerenderParentAlert` ~line 23; `applyDispatchCorrection` call ~line 255)
- Modify: `internal/transcribe/live_interpretation.go` (`publishLiveInterpretation` ~line 228-252; `badgeParentAlertSAR` ~line 314 → `refreshParentAlert`)
- Modify: `internal/transcribe/sweeper.go` (`updateAlertForClosure` ~line 200-216)
- Test: `internal/transcribe/brief_integration_test.go` (create; `DispatchSuite` methods)

**Interfaces:**
- Consumes: `FormatBrief`, `RescueTrailBlocksInput.Brief`, `readSummaryData`, `freshClosureMeta`, `seedActiveRescue` (test helper in corrections_test.go).
- Produces: `func (tc *TranscribeClient) rerenderParentAlert(ctx context.Context, meta ClosureMeta, fallback string) bool`; `func (tc *TranscribeClient) refreshParentAlert(ctx context.Context, tgid string, meta ClosureMeta, reason string)`.

- [ ] **Step 1: Write the failing tests** (create `internal/transcribe/brief_integration_test.go`)

```go
package transcribe

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/searchandrescuegg/transcribe/internal/ml"
	"github.com/slack-go/slack"
	"github.com/stretchr/testify/mock"
)

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
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -count=1 -run 'TestDispatchSuite/TestBrief_' ./internal/transcribe/...`
Expected: FAIL — no parent updates for brief changes; correction/closed renders lack the brief.

- [ ] **Step 3: Implement**

`internal/transcribe/corrections.go` — replace `rerenderParentAlert` with:

```go
// rerenderParentAlert rebuilds the live (not closed) rescue alert from meta plus the latest
// summary_data — transcription, dispatch-correction label, brief, SAR badge, status line, action
// buttons — and chat.updates it. It is the single live parent-alert render path: it derives the
// SAR badge and brief itself so no caller (summary pass, dispatch correction) can drop another's
// decoration. The current expiry comes from active_tacs; if it can't be read the rescue is
// closing, so skip. Best-effort: returns false on any failure.
func (tc *TranscribeClient) rerenderParentAlert(ctx context.Context, meta ClosureMeta, fallback string) bool {
	if meta.MessageTS == "" || meta.Transcription == "" {
		return false
	}
	score, err := tc.dragonflyClient.ZScore(ctx, activeTACsKey, meta.TGID)
	if err != nil {
		slog.Warn("parent alert re-render skipped; could not read expiry from active_tacs",
			slog.String("error", err.Error()), slog.String("tgid", meta.TGID))
		return false
	}
	summary, _ := tc.readSummaryData(ctx, meta.TGID)
	blocks := BuildRescueTrailBlocks(&RescueTrailBlocksInput{
		TACChannel:        meta.TACChannel,
		TranscriptionText: meta.Transcription,
		ExpiresAt:         time.Unix(int64(score), 0).Local(),
		DispatchTGID:      FireDispatch1TGID,
		TACTalkgroupTGID:  meta.TGID,
		SARNotified:       summary != nil && summary.SARNotified,
		Correction:        meta.DispatchCorrection,
		Brief:             FormatBrief(summary),
	})
	if err := tc.updateMessageWithRetry(ctx, meta.MessageTS,
		slack.MsgOptionBlocks(blocks...), slack.MsgOptionText(fallback, false)); err != nil {
		slog.Warn("parent alert re-render failed",
			slog.String("error", err.Error()), slog.String("tgid", meta.TGID), slog.String("message_ts", meta.MessageTS))
		return false
	}
	return true
}
```

In `applyDispatchCorrection`, change the call to:

```go
	tc.rerenderParentAlert(ctx, meta, fmt.Sprintf("%s — dispatch transcript corrected", meta.TACChannel))
```

`internal/transcribe/live_interpretation.go` — in `publishLiveInterpretation`, replace the `wasNotified` read with:

```go
	// Read the previous summary BEFORE overwriting summary_data, so we can detect the SAR
	// false→true transition and brief changes and re-render the parent alert only then.
	prevSummary, _ := tc.readSummaryData(ctx, tacTGID)
	wasNotified := prevSummary != nil && prevSummary.SARNotified
	prevBrief := FormatBrief(prevSummary)
```

and replace the `if summary.SARNotified && !wasNotified { tc.badgeParentAlertSAR(...) }` block (and its comment) with:

```go
	// Re-render the parent alert at most once per pass, and only when something visible on it
	// changed: the SAR badge latching on (false→true) or the page-out brief changing. The
	// re-render reads summary_data (written just above), so it carries both decorations.
	sarFlipped := summary.SARNotified && !wasNotified
	briefChanged := FormatBrief(summary) != prevBrief
	switch {
	case sarFlipped && briefChanged:
		tc.refreshParentAlert(ctx, tacTGID, meta, "sar_notified+brief_changed")
	case sarFlipped:
		tc.refreshParentAlert(ctx, tacTGID, meta, "sar_notified")
	case briefChanged:
		tc.refreshParentAlert(ctx, tacTGID, meta, "brief_changed")
	}
```

Replace `badgeParentAlertSAR` (keep and adapt its doc comment, including the fresh-meta rationale) with:

```go
func (tc *TranscribeClient) refreshParentAlert(ctx context.Context, tgid string, meta ClosureMeta, reason string) {
	meta = tc.freshClosureMeta(ctx, tgid, meta)
	fallback := fmt.Sprintf("%s — rescue alert updated", meta.TACChannel)
	if s, ok := tc.readSummaryData(ctx, tgid); ok {
		if brief := FormatBrief(s); brief != "" {
			fallback = fmt.Sprintf("%s — %s", meta.TACChannel, brief)
		} else if s.SARNotified {
			fallback = fmt.Sprintf("%s — Search & Rescue notified", meta.TACChannel)
		}
	}
	if tc.rerenderParentAlert(ctx, meta, fallback) {
		slog.Info("live interpretation: refreshed parent alert",
			slog.String("tgid", tgid), slog.String("tac", meta.TACChannel), slog.String("reason", reason))
	}
}
```

Update any test that called `badgeParentAlertSAR` directly to call `refreshParentAlert(..., "sar_notified")` (grep).

`internal/transcribe/sweeper.go` `updateAlertForClosure` — before building blocks:

```go
	summary, _ := tc.readSummaryData(ctx, m.TGID)
```

and in the `RescueTrailBlocksInput` replace `SARNotified: tc.summarySARNotified(ctx, m.TGID),` with:

```go
		// Preserve the SAR badge and the final brief on the closed alert.
		SARNotified: summary != nil && summary.SARNotified,
		Brief:       FormatBrief(summary),
```

If `summarySARNotified` has no remaining callers, delete it.

- [ ] **Step 4: Run tests to verify pass**

Run: `gofmt -l $(git ls-files '*.go'); go vet ./... && go test -count=1 ./internal/transcribe/...`
Expected: PASS, including `TestSARBadge_UsesFreshMetaAfterMidPassDispatchCorrection` and all existing SAR/sweeper tests. gofmt prints nothing.

- [ ] **Step 5: Commit**

```bash
git add internal/transcribe/
git commit -F - <<'EOF'
feat(transcribe): refresh the parent alert when the brief changes

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01UGf82rADsPjLxqirG637Y2
EOF
```

---

### Task 4: Docs and full verification

**Files:**
- Modify: `CLAUDE.md` (architecture table SAR-badge row ~line 49; critical invariants list; "Prompt iteration" section at the end)

- [ ] **Step 1: Architecture table** — add a row after the SAR-badge row:

```markdown
| Page-out brief on the parent alert, from the live summary | "Location · subject · condition" under the header so people see the situation at a glance; three `brief_*` slots on `RescueSummary` (prompt rule 17), formatted by `FormatBrief`. No brief until the first summary pass. `rerenderParentAlert` derives SAR badge + brief from `summary_data`, and `publishLiveInterpretation` re-renders only when the brief changes or SAR flips — at most one `chat.update` per pass, no new keys | `internal/prompts` (rule 17), `slack.go` (`FormatBrief`, `buildBriefBlock`), `live_interpretation.go` (`refreshParentAlert`), `corrections.go` (`rerenderParentAlert`), `sweeper.go` (closed-alert brief) |
```

and update the SAR-badge row's "Where" column reference from `badgeParentAlertSAR` to `refreshParentAlert`.

- [ ] **Step 2: Invariant** — append as the next number after the current last invariant:

```markdown
16. **`rerenderParentAlert` is the single live parent-alert render path and derives its
    decorations from `summary_data`** — the SAR badge and the brief are read inside it, never
    passed by callers. Otherwise a caller that only knows one decoration (e.g. a dispatch
    correction) re-renders the alert without the other and silently wipes it.
    (`corrections.go` `rerenderParentAlert`)
```

- [ ] **Step 3: Prompt iteration section** — add a sentence: "The `Brief*` fields (rule 17) feed the one-line brief on the parent alert via `FormatBrief`; changing their meaning changes what responders see first."

- [ ] **Step 4: Full verification**

Run: `gofmt -l $(git ls-files '*.go'); go vet ./... && go test -count=1 -timeout 5m ./...`
Expected: gofmt prints nothing; all packages PASS.

- [ ] **Step 5: Commit**

```bash
git add CLAUDE.md
git commit -F - <<'EOF'
docs: document the page-out brief and the single parent-alert render path

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01UGf82rADsPjLxqirG637Y2
EOF
```
