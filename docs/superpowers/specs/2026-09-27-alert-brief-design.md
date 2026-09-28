# Page-out-style brief on the rescue alert — design

Status: approved in design review 2026-09-27; spec pending review.
Sub-project 2 of 3 (1 = human corrections, PR #27; 3 = audio attachments).
Branch: `feat/alert-brief`, stacked on `feat/human-corrections` (reuses `rerenderParentAlert`).

## Goal

Give the parent rescue alert a ~6-word, page-out-style line — location · subject · condition —
so people see the situation at a glance without reading the long Live Interpretation.

Origin (scanner-channel feedback): "I'd love to see a ~6 word summary on the first page
(location, subject type, medical condition, like the page outs)"; human comments read clearer
than long bot messages; human-provided info takes priority over transcription.

## Decisions

| Question | Decision |
|---|---|
| Source | The Live Interpretation summary only. No brief until the first summary pass (first TAC transmission or `correction:` note). The dispatch parser is not changed. |
| Layout | Bold brief line directly under the alert header, above the SAR badge. The dispatch transcript stays in the alert (no change to correction targeting). |
| Shape | Three structured slots joined in code with ` · `; empty slots drop out. |
| Updates | Parent alert re-rendered only when the brief changes (or SAR flips) — at most one `chat.update` per summary pass. |
| Closed alert | Keeps the final brief. |
| Out of scope | Live Interpretation message layout, feedback-form prefill, dispatch-parser changes, moving the transcript to the thread. |

## Schema and prompt (`internal/ml`, `internal/prompts`)

`ml.RescueSummary` gains (after `Outcome`):

```go
// Brief* are the page-out-style slots rendered as one line on the parent alert
// ("Mailbox Peak · 54F · ankle injury"). Each is a few words; empty when unknown.
BriefLocation  string `json:"brief_location"`
BriefSubject   string `json:"brief_subject"`
BriefCondition string `json:"brief_condition"`
```

Both backends generate their structured-output schema from the struct via the shared
`internal/prompts` schema builder, so no per-backend change is needed; verify both backends'
schema tests still pass (strict-mode schemas require every property to be listed as required —
confirm the generator handles the new fields the same way as the existing string fields).

New rule 17 in `rescueSummarySystemPromptBase`:

> 17. BRIEF: Fill BriefLocation, BriefSubject, and BriefCondition as a dispatcher page-out would
> read — terse fragments, not sentences. BriefLocation: the best-known place in 1–3 words
> (trail, peak, trailhead, or road — e.g. "Mailbox Peak", "Rattlesnake Ledge"); use ONLY a place
> actually stated in the dispatch, transmissions, or operator corrections — never infer or
> invent one. BriefSubject: who needs help in 1–3 words — age+sex shorthand when known ("54F",
> "30M"), otherwise a count or role ("2 hikers", "hiker", "climber"). BriefCondition: the medical
> problem or situation in 1–3 words ("ankle injury", "cardiac", "lost, uninjured"). Leave any
> slot empty when it is not stated — never guess. Operator corrections override (rule 15).
> Keep the brief stable between updates; change a slot only when new information warrants it.

`renderPreviousSummary` adds a `Brief: <location> · <subject> · <condition>` line (dashes for
empty) so the additive pass sees and extends the prior brief.

## Rendering (`internal/transcribe/slack.go`)

- `func FormatBrief(s *ml.RescueSummary) string` — nil-safe; trims each slot, collapses
  internal whitespace/newlines, truncates each to 40 runes (`…`), mrkdwn-escapes `& < >`, strips
  `*` and `_` (they would break the bold wrapper), drops empty slots, joins with ` · `. Returns
  `""` when all slots are empty.
- `RescueTrailBlocksInput.Brief string`. When non-empty, a section block
  `*<brief>*` (mrkdwn) is inserted directly after the header — before the SAR badge. Empty
  `Brief` ⇒ blocks byte-identical to today (existing exact-JSON tests unchanged).
- Resulting order: header, brief, SAR badge, divider, channel, transcript, correction label,
  audio button, divider, status, actions/feedback.

## Update policy (`internal/transcribe/live_interpretation.go`, `corrections.go`)

- **`rerenderParentAlert` derives its decorations itself.** Signature becomes
  `rerenderParentAlert(ctx, meta ClosureMeta, fallback string) bool`; it reads `summary_data`
  (`readSummaryData`) and renders `SARNotified` and `FormatBrief(summary)` from it. Every caller —
  SAR transition, brief change, dispatch correction — therefore carries every decoration; no
  path can wipe another's. (`applyDispatchCorrection` drops its `summarySARNotified` argument.)
- **Change detection, no new keys.** `publishLiveInterpretation` already reads the previous
  summary before overwriting `summary_data` (for the SAR transition). It reads the previous
  `FormatBrief` at the same point. After writing the new `summary_data`:
  - `sarFlipped := summary.SARNotified && !wasNotified`
  - `briefChanged := FormatBrief(summary) != prevBrief`
  - if either, call one re-render (via the existing fresh-meta path, `freshClosureMeta`), with
    fallback text `"<TAC> — <brief>"` when a brief exists, else the existing SAR text.
  - `badgeParentAlertSAR` is generalized/renamed to `refreshParentAlert(ctx, tgid, meta, reason)`;
    log the reason (`sar_notified`, `brief_changed`, both).
- A rewrite pass (no PreviousSummary) may change the brief; that is an ordinary change and
  re-renders once.

## Closed alert (`internal/transcribe/sweeper.go`)

`updateAlertForClosure` already reads `summary_data` before cleanup (invariant #4). It passes
`Brief: FormatBrief(summary)` alongside the existing `SARNotified` (single `readSummaryData`
read instead of the current `summarySARNotified` helper call).

## Error handling

Best-effort throughout, matching existing re-render behavior: a failed `chat.update` logs and
the alert catches up on the next brief change; the Live Interpretation post is unaffected. A
missing/unparseable `summary_data` renders no brief and no SAR badge (today's behavior for SAR).

## Testing

- **Unit (`slack_brief_test.go`)**: `FormatBrief` — all slots, empty slots dropped, all empty ⇒
  `""`, nil summary, whitespace/newline collapse, 40-rune truncation, `& < >` escaped, `*`/`_`
  stripped. Block builder — empty brief byte-identical to current output; brief inserted at
  index 1; brief + SAR badge + correction label all present in the documented order.
- **Prompts**: rule 17 present (incl. "never infer or invent"); `renderPreviousSummary` emits the
  `Brief:` line.
- **Integration (DispatchSuite)**:
  - first summary with a brief ⇒ exactly one parent `UpdateMessageContext`;
  - second pass, identical brief, no SAR flip ⇒ no parent update;
  - pass with changed brief ⇒ one update, rendered blocks (decoded via
    `slack.UnsafeApplyMsgOptions`) contain the new brief;
  - SAR flip + brief change in the same pass ⇒ exactly one update containing both;
  - dispatch correction after a brief exists ⇒ re-render still contains the brief;
  - sweeper close ⇒ closed alert contains the brief.

## Docs

CLAUDE.md: extend the SAR-badge architecture row (or add one) describing the brief and the
"re-render derives SAR + brief from `summary_data`, one update per pass, only on change" rule;
note in the invariants that `rerenderParentAlert` must stay the single parent-alert render path.
Prompt-iteration section: mention rule 17 and that `Brief*` fields feed the parent alert.
