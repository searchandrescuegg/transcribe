// Package prompts is the single source of truth for the LLM prompt + response-schema
// contract shared by every ML backend (OpenAI-compatible and Anthropic). Keeping the
// prompt text and JSON schemas here — rather than inside one backend package — guarantees
// the two backends can't silently drift apart on wording or output shape.
//
// Iterating on a prompt: edit the constants below, then re-run the iteration CLIs
// (cmd/test-transcription, cmd/test-summary). Changes take effect on the next binary build
// regardless of which backend is wired in.
package prompts

import (
	"fmt"
	"strings"

	"github.com/searchandrescuegg/transcribe/internal/ml"
)

// UnknownCallType is appended to any caller-supplied allowed-call-types list so the model
// always has an escape hatch when it can't classify, instead of being forced to fabricate.
const UnknownCallType = "Unknown"

// DispatchSystemPrompt returns the system prompt for the dispatch parser. When
// allowedCallTypes is non-empty the prompt is rewritten to reference the canonical list
// (the list itself is inlined so the model knows the exact spelling and casing it MUST
// emit); an empty list keeps the in-line example call types for backward compatibility.
// The King County place-name gazetteer is appended in both cases so the model can correct
// garbled local locations in the cleaned transcription.
func DispatchSystemPrompt(allowedCallTypes []string) string {
	var b strings.Builder
	if len(allowedCallTypes) == 0 {
		b.WriteString(defaultSystemPrompt)
	} else {
		// Use a clearly-delimited list so the model can't misinterpret comma-separated names
		// that themselves contain commas (e.g. "Rescue - Trail, Mountain"). One per line.
		b.WriteString(constrainedSystemPromptHead)
		b.WriteString("\nThe call_type field MUST be exactly one of the following (case-sensitive):\n")
		for _, ct := range allowedCallTypes {
			b.WriteString("- ")
			b.WriteString(ct)
			b.WriteString("\n")
		}
		b.WriteString("- ")
		b.WriteString(UnknownCallType)
		b.WriteString("\n")
		b.WriteString(constrainedSystemPromptTail)
	}
	b.WriteString("\n\n")
	b.WriteString(kingCountyGazetteer)
	return b.String()
}

const defaultSystemPrompt = `You are a tool to accurately parse relevant information from a transcription of Fire Department radio messages.
You will need to extract the call type and the tactical channel (TAC) from the transcription, including the FULL transcription.
Please return the information in the defined format. There may be multiple calls within a single transcription, so if there are multiple calls, please identify and separate into multiple messages, but ensure they are deduplicated.
Call types can include "Aid Emergency", "MVC", "MVC Aid Emergency", "AFA Commercial", "Rescue - Trail", etc.
If the call type can not be determined, return "Unknown".
When the dispatch announces a "Rescue Trail" (a trail, wilderness, or hiker rescue — often "Rescue Trail TAC N"), classify it specifically as the trail-rescue call type ("Rescue - Trail"), NOT a generic rescue type like "Rescue - General". This distinction routes real trail rescues, so err toward "Rescue - Trail" whenever "Rescue Trail" is spoken.
The tactical channel (TAC) should be in the format "TAC1", "TAC2", etc. Do not include a space between "TAC" and the number. If it appears as SPFR Repeater, assume it is "TAC8".
Please clean the transcription to update any misspellings, incorrect locations, and generally ensure that it is clear and concise.
Do not add any additional information or context that is not present in the transcription.`

const constrainedSystemPromptHead = `You are a tool to accurately parse relevant information from a transcription of Fire Department radio messages.
You will need to extract the call type and the tactical channel (TAC) from the transcription, including the FULL transcription.
Please return the information in the defined format. There may be multiple calls within a single transcription, so if there are multiple calls, please identify and separate into multiple messages, but ensure they are deduplicated.`

const constrainedSystemPromptTail = `If the call type cannot be confidently mapped to one of the values listed above, return "Unknown".
When the dispatch announces a "Rescue Trail" (a trail, wilderness, or hiker rescue — often "Rescue Trail TAC N"), classify it specifically as the trail-rescue call type (the listed value containing both "Rescue" and "Trail"), NOT a generic rescue type. This distinction routes real trail rescues, so err toward the trail-rescue value whenever "Rescue Trail" is spoken.
The tactical channel (TAC) should be in the format "TAC1", "TAC2", etc. Do not include a space between "TAC" and the number. If it appears as SPFR Repeater, assume it is "TAC8".
Please clean the transcription to update any misspellings, incorrect locations, and generally ensure that it is clear and concise.
Do not add any additional information or context that is not present in the transcription.`

// rescueSummarySystemPromptBase is the canonical summarizer instruction; the King County
// gazetteer is appended to it to form RescueSummarySystemPrompt below.
// Iterate on this string to tune the summary's voice, completeness, and accuracy. Pair
// edits with runs of cmd/test-summary against fixture inputs to see the effect.
const rescueSummarySystemPromptBase = `You are an emergency-response analyst summarizing radio chatter from a US fire department's tactical channel during an in-progress rescue.

You will receive:
  - The original dispatch transcription that initiated the rescue (anchors what kind of incident this is).
  - The dispatched call type (e.g. "Rescue - Trail") and the assigned tactical channel (e.g. "TAC10").
  - An ordered list of TAC channel transmissions (one per radio key-up), each with a capture timestamp.
  - Optionally, your PREVIOUS summary from the last update (to be extended, not rewritten).
  - Optionally, a list of units currently assigned to the call from CAD (to canonicalize unit callsigns).

Produce a structured summary that lets a human responder catch up at a glance. Follow these rules strictly:

1. Be concise and factual. Do not speculate about details not stated in the transcripts.
2. The transcripts come from imperfect speech-to-text. Normalize obvious mistakes: "Italian one seventy one" → "Battalion 171"; "Mabel Valley" → "Maple Valley"; numeric units like "8171" likely mean Battalion 8171 or Engine 8171 — preserve as written if context is ambiguous.
3. Headline: one short sentence (≤ 80 chars) capturing the situation as it currently stands. Aim for what the on-call would want to know first.
4. SituationSummary: 1–3 sentences. What's the incident, where, who's responding, what's happening operationally.
5. Location: name the best-known location from the chatter (trailhead, address, mile marker). Empty string if not stated.
6. UnitsInvolved: list every responding unit mentioned (e.g. "Engine 171", "Aid 151", "Battalion 171", "Medic 104"). Deduplicate. Use the canonical form, not the spoken form.
7. PatientStatus: one short phrase about patient condition or transport ("ambulatory; refused transport", "transported to Overlake", "no patient contact"). Empty if unstated.
8. Outcome: short phrase describing the disposition of the INCIDENT itself, not of individual units. Common values: "Ongoing", "Resolved — patient transported", "Resolved — handled on scene", "Cancelled en route", "False alarm". Carefully distinguish canceled RESOURCES from a canceled INCIDENT:
  - If only additional or mutual-aid units are canceled (e.g. "discontinue the request for Maple Valley units", "wave-firm you're canceled") while a unit remains on scene with the patient, the incident is NOT cancelled — it is ongoing or resolved on scene. Note the canceled units in the summary, but do not report the Outcome as "Cancelled".
  - Use "Cancelled en route" ONLY when the entire response is called off before any unit makes patient contact (e.g. the reporting party cancels, or dispatch cancels all responding units).
  - "Code 4", "handled", or the primary unit clearing scene indicates the incident resolved.
9. KeyEvents: an ordered list of notable moments with their CapturedAt timestamp from the transmission. Examples: "13:24:00 — Engine 8171 arrived on scene", "13:25:30 — Patient reached, ambulatory", "13:27:15 — Maple Valley resources canceled". Aim for 3–8 events; skip small acknowledgements ("copy", "171 received").
10. SARNotified: set to true ONLY if the chatter clearly indicates Search and Rescue (SAR) has been notified, requested, contacted, or is responding. The phrasing varies widely — treat all of these (and similar) as SAR notification: "notifying SAR", "SAR has been notified", "calling Search and Rescue", "requesting SAR", "SAR is en route", "King County SAR contacted", "advised Search and Rescue", "SAR activated". Set it to false if SAR is not mentioned, or is only raised as a possibility ("we might need SAR", "stand by for a SAR request") without an actual notification. Do not infer SAR involvement from a generic rescue response — require an explicit reference to Search and Rescue / SAR being contacted or responding.
11. If the transcripts are too sparse to populate a field, return an empty string (or empty list for arrays, or false for booleans) rather than fabricating.
12. ADDITIVE UPDATES: When a PREVIOUS SUMMARY is provided, treat it as the established record. Extend it — do not re-derive it from scratch. Specifically: preserve every prior KeyEvent verbatim and in its original order, then append new events from transmissions that arrived since. Do NOT drop, reorder, or reword an existing KeyEvent unless a later transmission explicitly corrects or contradicts it (in which case add a new event noting the correction rather than silently editing history). Headline, SituationSummary, PatientStatus, and Outcome SHOULD be refreshed to reflect the latest state, but only change them when the new transmissions actually warrant it — stability between updates is a feature. If no PREVIOUS SUMMARY is provided, produce a fresh summary as usual.
13. UNIT CANONICALIZATION: When a "Units currently assigned" list is provided, use it as ground truth for the UnitsInvolved field and for interpreting garbled unit callsigns in the transcripts (e.g. a transmission that sounds like "eighty one seventy one" resolves to a listed unit "A8171"). Never add a unit to UnitsInvolved solely because it appears in the assigned list — only include units that the transcripts actually reference; the list is for spelling/disambiguation, not for inventing participation.
14. CALL TIMER: A spoken time reference like "Time 1:42" (or "time one forty two") is an ELAPSED call timer — time since dispatch — NOT a wall-clock time. Do not convert it to a 24-hour clock or a time of day, and do not use it as a KeyEvents timestamp. KeyEvents timestamps come ONLY from the CapturedAt value attached to each transmission.`

// RescueSummarySystemPrompt is the base summarizer prompt with the King County place-name
// gazetteer appended, so the model corrects garbled local locations in the Location field
// and key events.
var RescueSummarySystemPrompt = rescueSummarySystemPromptBase + "\n\n" + kingCountyGazetteer

// kingCountyGazetteer is a baked-in reference of place names in the King County / I-90
// (Snoqualmie) corridor that dominate trail-rescue traffic. Speech-to-text reliably garbles
// these — especially Native American and short names — so the prompt uses the list to snap
// phonetic near-misses back to canonical spellings. It is framed conservatively (correct only
// clear matches; never force one) to avoid rewriting locations that were transcribed correctly
// but aren't listed. This is public geography, so it lives in the prompt rather than the
// encrypted call-types file. It rarely changes; when you edit it the prompt_hash recorded in
// the dataset updates automatically, so you can A/B the effect. Appended to both the dispatch
// and rescue-summary system prompts.
const kingCountyGazetteer = `=== Local place-name reference (King County & the I-90 / Snoqualmie corridor) ===
Speech-to-text frequently garbles the proper nouns below, especially Native American and short names (for example "Mount Sai" or "Mount Sigh" should be "Mount Si"; "Squallamie" or "Snowqualmie" should be "Snoqualmie"; "Sammish" should be "Sammamish"; "Kalitan" should be "Kaleetan"; "Melaqua" should be "Melakwa"). When a transcription clearly corresponds to a name below but is phonetically garbled or misspelled, correct it to the canonical form shown. "Mt." and "Mount" are interchangeable and do not need to be normalized — leave whichever form the transcription used — with one exception: Mount Si must always be written "Mount Si", never "Mt. Si" or just "Si".

CRITICAL — location safety rule: Correct a place name to a listed entry ONLY when the transcription is an OBVIOUS PHONETIC NEAR-MATCH to that entry (they must sound clearly alike, as in the "Mount Sai" → "Mount Si" examples above). NEVER replace a place name with a DIFFERENT real place: if the transcribed location is not an obvious near-match to a listed name, keep it EXACTLY as transcribed — even if it resembles no place on this list, and even if some listed place seems "close enough." Do not swap "Norway Hill" for "Rattlesnake Ledge", or any name for a differently-sounding one, just to land on a known location. A wrong location misdirects emergency responders, so when in any doubt, preserve the transcribed name verbatim and do NOT force a match or invent a location.

Cities & towns: Seattle, Bellevue, Renton, Kent, Auburn, Federal Way, Kirkland, Redmond, Sammamish, Issaquah, Snoqualmie, North Bend, Enumclaw, Maple Valley, Covington, Black Diamond, Duvall, Carnation, Fall City, Preston, Hobart, Ravensdale, Newcastle, Woodinville, Bothell, Kenmore, Shoreline, Burien, SeaTac, Tukwila, Mercer Island, Vashon, Skykomish, Baring, Kanaskat, Palmer.

Peaks, trails & trailheads: Mount Si, Little Si, Mount Teneriffe, Teneriffe Falls, Mailbox Peak, Rattlesnake Ledge, Rattlesnake Mountain, West Tiger Mountain, East Tiger Mountain, Poo Poo Point, Squak Mountain, Cougar Mountain, Cedar Butte, Grand Ridge, Taylor Mountain, Granite Mountain, Mount Washington, McClellan Butte, Bandera Mountain, Mount Defiance, Ira Spring Trail, Mason Lake, Snow Lake, Source Lake, Kendall Katwalk, Snoqualmie Mountain, Guye Peak, Chair Peak, Denny Mountain, Alpental, The Tooth, Kaleetan Peak, Chikamin Peak, Silver Peak, Humpback Mountain, Tinkham Peak, Bessemer Mountain, Dirty Harry's Peak, Dirty Harry's Balcony, Annette Lake, Talapus Lake, Olallie Lake, Pratt Lake, Melakwa Lake.

Rivers, lakes & falls: Snoqualmie River (North Fork, Middle Fork, South Fork), Cedar River, Green River, White River, Raging River, Tolt River, Lake Washington, Lake Sammamish, Rattlesnake Lake, Keechelus Lake, Kachess Lake, Snoqualmie Falls, Twin Falls, Franklin Falls, Weeks Falls, Denny Creek.

Roads & highways: Interstate 90 (I-90), Interstate 5 (I-5), Interstate 405 (I-405), SR 18, SR 202, SR 203, SR 169 (Maple Valley Highway), SR 410, US 2 (Stevens Pass), SR 900, Snoqualmie Pass, Middle Fork Road, Mount Si Road, North Bend Way, Cedar Falls Road, Issaquah-Hobart Road, Preston-Fall City Road.`

// BuildRescueSummaryUserPrompt formats the input as a clearly-delimited block. The model
// performs better when the dispatch and TAC sections are explicitly labeled.
func BuildRescueSummaryUserPrompt(input ml.RescueSummaryInput) string {
	var b strings.Builder
	b.WriteString("=== DISPATCH ===\n")
	if input.DispatchCallType != "" || input.TACChannel != "" {
		fmt.Fprintf(&b, "Call type: %s\n", emptyAsDash(input.DispatchCallType))
		fmt.Fprintf(&b, "TAC channel: %s\n", emptyAsDash(input.TACChannel))
		b.WriteString("\n")
	}
	b.WriteString("Transcript:\n")
	b.WriteString(input.DispatchTranscription)

	if input.UnitContext != "" {
		b.WriteString("\n\n")
		b.WriteString(input.UnitContext)
	}

	if input.PreviousSummary != nil {
		b.WriteString("\n\n=== PREVIOUS SUMMARY (extend, do not rewrite) ===\n")
		b.WriteString(renderPreviousSummary(input.PreviousSummary))
	}

	b.WriteString("\n\n=== TAC TRANSMISSIONS (chronological) ===\n")
	if len(input.TACTranscripts) == 0 {
		b.WriteString("(none yet)\n")
	}
	for i, t := range input.TACTranscripts {
		fmt.Fprintf(&b, "[%d] %s — %s\n", i+1, emptyAsDash(t.CapturedAt), t.Text)
	}
	return b.String()
}

// renderPreviousSummary serializes the prior RescueSummary into the same labeled shape the
// model emits, so it reads its own output back and extends it rather than re-deriving. KeyEvents
// are rendered in order and explicitly flagged as established so the additive rule has something
// concrete to preserve.
func renderPreviousSummary(s *ml.RescueSummary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Headline: %s\n", emptyAsDash(s.Headline))
	fmt.Fprintf(&b, "SituationSummary: %s\n", emptyAsDash(s.SituationSummary))
	fmt.Fprintf(&b, "Location: %s\n", emptyAsDash(s.Location))
	fmt.Fprintf(&b, "UnitsInvolved: %s\n", emptyAsDash(strings.Join(s.UnitsInvolved, ", ")))
	fmt.Fprintf(&b, "PatientStatus: %s\n", emptyAsDash(s.PatientStatus))
	fmt.Fprintf(&b, "Outcome: %s\n", emptyAsDash(s.Outcome))
	fmt.Fprintf(&b, "SARNotified: %t\n", s.SARNotified)
	b.WriteString("KeyEvents (established — preserve these verbatim, then append):\n")
	if len(s.KeyEvents) == 0 {
		b.WriteString("  (none yet)\n")
	}
	for _, e := range s.KeyEvents {
		fmt.Fprintf(&b, "  - %s — %s\n", emptyAsDash(e.CapturedAt), e.Description)
	}
	return b.String()
}

// TACCleanupSystemPrompt instructs the model to clean up a single raw TAC transmission. Like the
// dispatch parser it corrects ASR errors and place names (via the shared gazetteer) but is
// strictly faithful — it never adds facts. It also uses the (optional) assigned-unit roster to
// pin garbled unit callsigns.
var TACCleanupSystemPrompt = tacCleanupSystemPromptBase + "\n\n" + kingCountyGazetteer

const tacCleanupSystemPromptBase = `You are a transcription editor for a US fire department's tactical (TAC) radio channel during an in-progress rescue. You are given one raw speech-to-text transcription of a single radio transmission, plus context about the incident. Return a cleaned version of that ONE transmission.

Rules:
1. Fix obvious speech-to-text errors so the transmission reads clearly and correctly: mis-heard words, run-together phrases, wrong homophones, and mangled numbers ("seventy three" → "73"). Use the incident context to disambiguate.
2. Correct radio and agency shorthand to its standard form: tactical channels as "TAC2" (no space), "KCSO" for the King County Sheriff's Office, "KCSAR"/"King County SAR" for Search and Rescue, "Battalion", "Engine", "Aid", "Medic", "Ladder", "Rescue", etc.
3. Correct unit callsigns to their canonical form. If a "Units currently assigned" list is provided, prefer callsigns from that list when the audio is a close phonetic match (e.g. "eighty one seventy one" → a listed "A8171"). Do not invent a unit that is neither spoken nor listed.
4. Correct garbled place names using the local place-name reference below, but ONLY on an obvious phonetic near-match. Never substitute one real place for a different-sounding one, and never invent a location — see the CRITICAL location safety rule in that reference. A wrong location misdirects responders, so when unsure, keep the location exactly as transcribed.
5. Be strictly faithful. Do NOT add information, context, units, or events that are not present in the raw transmission. Do NOT summarize, editorialize, or drop content. Preserve the speaker's meaning, order, and terseness.
6. Correct a word ONLY when your correction is an obvious PHONETIC or spelling fix of what was actually said — the correction must sound like the transcribed word (e.g. "KTSO" → "KCSO", "Italian one seventy one" → "Battalion 171"). Do NOT reinterpret a garbled word into a different, semantically-plausible term that does NOT sound like it: for example, do NOT turn "Cadres provided" into "Grid reference provided" or "Coordinates provided", and do NOT turn "Cremio" into "primary". Guessing a domain-plausible word from context is a fabrication. If no phonetic correction clearly fits, keep the transcribed word verbatim — a faithful garble is better than a confident fabrication.
7. A time reference at the end of a transmission (e.g. "time one forty two", "Time 1:42") is an ELAPSED call timer — the time since dispatch — NOT a time of day. Render it as spoken minutes:seconds (e.g. "Time 1:42"); never convert it to a 24-hour clock (e.g. "1342") or a wall-clock time.
8. Return only the cleaned transcription text in the cleaned_text field — no commentary.`

// BuildTACCleanupUserPrompt formats the raw transmission plus its incident + unit context into a
// clearly-delimited block for the cleanup call.
func BuildTACCleanupUserPrompt(input ml.TACCleanupInput) string {
	var b strings.Builder
	if input.DispatchContext != "" {
		b.WriteString("=== INCIDENT CONTEXT (dispatch that started this rescue) ===\n")
		b.WriteString(input.DispatchContext)
		b.WriteString("\n\n")
	}
	if input.UnitContext != "" {
		b.WriteString(input.UnitContext)
		b.WriteString("\n\n")
	}
	b.WriteString("=== RAW TRANSMISSION TO CLEAN ===\n")
	b.WriteString(input.Text)
	return b.String()
}

func emptyAsDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
