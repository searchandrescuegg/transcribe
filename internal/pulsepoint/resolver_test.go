package pulsepoint

import (
	"sort"
	"testing"
	"time"

	"github.com/michaelpeterswa/pulpo"
	"github.com/stretchr/testify/assert"
)

func sortedUnitIDs(units []UnitInfo) []string {
	ids := make([]string, 0, len(units))
	for _, u := range units {
		ids = append(ids, u.ID)
	}
	sort.Strings(ids)
	return ids
}

var refTime = time.Date(2026, 7, 14, 14, 2, 0, 0, time.UTC)

func TestSelectUnitContext_MatchesByLocationAndCallType(t *testing.T) {
	active := []pulpo.Incident{
		{
			ID:                 "inc-noise",
			CallType:           "TC", // traffic collision, different location
			FullDisplayAddress: "Interstate 405 near Coal Creek Parkway",
			Unit:               []pulpo.Unit{{UnitID: "E999", DispatchStatus: "ER"}},
		},
		{
			ID:                   "inc-rescue",
			CallType:             "RESCUE",
			FullDisplayAddress:   "Mount Si Trailhead, Southeast Mount Si Road, North Bend",
			CallReceivedDateTime: "2026-07-14T14:01:30Z",
			Unit: []pulpo.Unit{
				{UnitID: "B171", DispatchStatus: "OS"},
				{UnitID: "E171", DispatchStatus: "ER"},
			},
		},
	}
	legend := map[string]string{"ER": "En Route", "OS": "On Scene"}

	// Dispatch text shares "mount", "trailhead", "road" tokens with the rescue incident.
	got := selectUnitContext(active, legend, "Rescue Trail Mount Si Trailhead Southeast Mount Si Road", refTime)

	assert.True(t, got.Matched, "should confidently match the rescue incident")
	assert.Equal(t, "inc-rescue", got.IncidentID)
	assert.Equal(t, []string{"B171", "E171"}, sortedUnitIDs(got.Units))

	block := got.PromptBlock()
	assert.Contains(t, block, "B171 (On Scene)", "status decoded via legend")
	assert.Contains(t, block, "E171 (En Route)")
	assert.Contains(t, block, "Matched incident")
}

func TestSelectUnitContext_FallsBackToRescueRosterWhenNoConfidentMatch(t *testing.T) {
	active := []pulpo.Incident{
		{ID: "a", CallType: "RESCUE", FullDisplayAddress: "somewhere far", Unit: []pulpo.Unit{{UnitID: "E1"}}},
		{ID: "b", CallType: "MEDICAL", FullDisplayAddress: "elsewhere entirely", Unit: []pulpo.Unit{{UnitID: "L2"}, {UnitID: "E1"}}},
	}

	// Dispatch text overlaps no location token → no confident match (call-type alone can't win).
	got := selectUnitContext(active, map[string]string{}, "unrelated words zzzz", refTime)

	assert.False(t, got.Matched, "no location overlap → roster fallback")
	assert.Equal(t, []string{"E1", "L2"}, sortedUnitIDs(got.Units), "union of rescue-like incidents, deduped")

	block := got.PromptBlock()
	assert.Contains(t, block, "No single incident matched")
	assert.Contains(t, block, "E1")
}

func TestSelectUnitContext_FallbackExcludesNonRescueIncidents(t *testing.T) {
	active := []pulpo.Incident{
		{ID: "a", CallType: "TC", FullDisplayAddress: "far", Unit: []pulpo.Unit{{UnitID: "E1"}}},     // traffic — excluded
		{ID: "b", CallType: "AFA", FullDisplayAddress: "far", Unit: []pulpo.Unit{{UnitID: "L2"}}},    // alarm — excluded
		{ID: "c", CallType: "RESCUE", FullDisplayAddress: "far", Unit: []pulpo.Unit{{UnitID: "R3"}}}, // included
	}

	got := selectUnitContext(active, map[string]string{}, "unrelated zzzz", refTime)

	assert.False(t, got.Matched)
	assert.Equal(t, []string{"R3"}, sortedUnitIDs(got.Units), "only rescue/medical units survive the fallback")
}

func TestSelectUnitContext_CallTypeAndRecencyAloneDoNotMatch(t *testing.T) {
	// A recent RESCUE incident (type +1.0, recency ~+1.0 = 2.0 ≥ threshold) but ZERO location
	// overlap must NOT be taken as a confident match — it drops to the roster fallback instead.
	active := []pulpo.Incident{{
		ID:                   "rescue-elsewhere",
		CallType:             "RESCUE",
		FullDisplayAddress:   "Completely Different Place",
		CallReceivedDateTime: "2026-07-14T14:02:00Z", // simultaneous with refTime
		Unit:                 []pulpo.Unit{{UnitID: "R9"}},
	}}

	got := selectUnitContext(active, map[string]string{}, "mount si trailhead", refTime)

	assert.False(t, got.Matched, "no shared location token → not a confident match")
	assert.Equal(t, []string{"R9"}, sortedUnitIDs(got.Units), "still surfaced via the rescue roster fallback")
}

func TestSelectUnitContext_EmptyWhenNoActiveIncidents(t *testing.T) {
	got := selectUnitContext(nil, map[string]string{}, "anything", refTime)
	assert.False(t, got.Matched)
	assert.Empty(t, got.Units)
	assert.Equal(t, "", got.PromptBlock(), "zero context renders no block")
}

func TestPromptBlock_EmptyForZeroValue(t *testing.T) {
	assert.Equal(t, "", UnitContext{}.PromptBlock())
}

func TestPromptBlock_RawStatusWhenLegendMissing(t *testing.T) {
	uc := UnitContext{
		Matched: true,
		Units:   []UnitInfo{{ID: "A181", Status: "XX"}},
	}
	assert.Contains(t, uc.PromptBlock(), "A181 (XX)", "raw status when legend can't decode")
}
