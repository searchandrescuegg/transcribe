package pulsepoint

import (
	"fmt"
	"strings"
)

// UnitInfo is one unit assigned to a call, with its dispatch status decoded to human-readable
// form via the agency unit legend (falls back to the raw status code when the legend lacks it).
type UnitInfo struct {
	ID     string // canonical callsign, e.g. "A181", "B181", "L161"
	Status string // decoded dispatch status, e.g. "En Route", "On Scene"
}

// UnitContext is the resolved set of units feeding the cleanup + summary prompts. Matched
// distinguishes a confident single-incident match from the agency-wide active roster fallback.
type UnitContext struct {
	Matched    bool
	IncidentID string
	CallType   string
	Address    string
	Units      []UnitInfo
}

// PromptBlock renders the unit context as a labeled block for inclusion in an LLM prompt. Returns
// an empty string when there are no units, so callers can concatenate unconditionally without
// emitting an empty header.
func (u UnitContext) PromptBlock() string {
	if len(u.Units) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("=== Units currently assigned to this call (from CAD) ===\n")
	if u.Matched {
		header := "Matched incident"
		if u.CallType != "" {
			header += ": " + u.CallType
		}
		if u.Address != "" {
			header += " — " + u.Address
		}
		b.WriteString(header)
		b.WriteString("\n")
	} else {
		b.WriteString("(No single incident matched confidently — showing all units active for the agency; use these callsigns for spelling/disambiguation only.)\n")
	}
	b.WriteString("Use these exact callsigns to correct garbled unit references. Do not add a unit that the radio traffic does not mention.\n")
	for _, unit := range u.Units {
		if unit.Status != "" {
			fmt.Fprintf(&b, "  - %s (%s)\n", unit.ID, unit.Status)
		} else {
			fmt.Fprintf(&b, "  - %s\n", unit.ID)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
