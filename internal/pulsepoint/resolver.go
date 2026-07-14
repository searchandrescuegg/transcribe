// Package pulsepoint resolves the units assigned to an in-progress rescue from the PulsePoint
// CAD feed (via the pulpo client) and renders them as a prompt block. The transcribe service
// feeds that block into the per-transmission cleanup and the live summary so garbled unit
// callsigns can be canonicalized against the units actually on the call.
//
// Correlating a radio rescue to a specific CAD incident is inherently fuzzy, so ResolveForRescue
// scores active incidents by location overlap (with the dispatch text), call-type, and recency,
// and falls back to the union of all active-agency units when no single incident matches
// confidently. Everything here is best-effort: the caller treats any error as "no unit context".
package pulsepoint

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/michaelpeterswa/pulpo"
)

// Options configures a Resolver.
type Options struct {
	BaseURL  string
	APIKey   string
	AgencyID string
	// Username / Password are the HTTP Basic auth credentials the PulsePoint API requires
	// alongside the apikey. When both are non-empty the client sends them; leave empty to skip.
	Username string
	Password string
	// Timeout bounds each CAD round-trip independently of the caller's context so a slow feed
	// can't eat the worker budget. Zero uses a 5s default.
	Timeout time.Duration
}

// Resolver queries the CAD feed and builds UnitContexts. Safe for concurrent use.
type Resolver struct {
	client   *pulpo.Client
	agencyID string
	timeout  time.Duration

	// The dispatch-status legend rarely changes, so it's cached process-wide behind a mutex.
	legendMu      sync.Mutex
	legend        map[string]string
	legendExpires time.Time
}

const (
	defaultTimeout     = 5 * time.Second
	legendCacheTTL     = 1 * time.Hour
	recencyWindow      = 30 * time.Minute // incidents older than this contribute no recency score
	matchScoreThresh   = 2.0              // minimum score to accept a single-incident match
	locationTokenMinLn = 4                // ignore short/common tokens when comparing locations
)

// rescueCallTypeHints are substrings (case-insensitive) in an incident's CallType that suggest a
// trail-rescue / medical incident. PulsePoint call-type codes vary by agency, so this is a soft
// boost, never a hard filter. Keep each hint specific enough that it won't collide with unrelated
// call types (e.g. avoid a bare "TR", which matches "TRAFFIC"/"STRUCTURE").
var rescueCallTypeHints = []string{"RESCUE", "MEDICAL", "MEDIC", "TRAUMA", "INJURY", "FALL"}

// NewResolver constructs a Resolver. It panics only on an invalid base URL / empty API key, which
// are startup misconfigurations; main.go validates presence before calling.
func NewResolver(opts Options) *Resolver {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	clientOpts := []pulpo.Option{
		pulpo.WithBaseURL(opts.BaseURL),
		pulpo.WithAPIKey(opts.APIKey),
		pulpo.WithHTTPClient(&http.Client{Timeout: timeout}),
	}
	// PulsePoint gates the API behind Basic auth in addition to the apikey; send it when both
	// credentials are present.
	if opts.Username != "" && opts.Password != "" {
		clientOpts = append(clientOpts, pulpo.WithBasicAuth(opts.Username, opts.Password))
	}

	client, err := pulpo.NewClient(clientOpts...)
	if err != nil {
		// Construction only fails on empty base URL / API key, both validated upstream. Fall back
		// to a nil client; RescueUnitBlock guards on it and returns empty context.
		return &Resolver{agencyID: opts.AgencyID, timeout: timeout}
	}

	return &Resolver{
		client:   client,
		agencyID: opts.AgencyID,
		timeout:  timeout,
	}
}

// RescueUnitBlock resolves the units for a rescue and returns the rendered prompt block (empty
// when nothing is found). Satisfies transcribe.UnitResolver.
func (r *Resolver) RescueUnitBlock(ctx context.Context, dispatchText string, referenceTime time.Time) (string, error) {
	uc, err := r.ResolveForRescue(ctx, dispatchText, referenceTime)
	if err != nil {
		return "", err
	}
	return uc.PromptBlock(), nil
}

// ResolveForRescue fetches active incidents + the status legend, then selects the best matching
// incident (or the agency-wide roster fallback). Returns a zero UnitContext (never nil) when the
// feed is empty; returns an error only when the incident fetch itself fails.
func (r *Resolver) ResolveForRescue(ctx context.Context, dispatchText string, referenceTime time.Time) (UnitContext, error) {
	if r.client == nil {
		return UnitContext{}, nil
	}

	callCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	// Both:true (the `both=1` query param) is REQUIRED. Without it the PulsePoint feed returns a
	// flat `{"incidents":[]}` array that does NOT unmarshal into the bucketed {alerts,active,recent}
	// shape pulpo expects, so List silently yields nothing. With it we get the buckets and read
	// Active (in-progress incidents); Recent holds closed calls we don't need.
	resp, err := r.client.Incidents.List(callCtx, r.agencyID, &pulpo.IncidentListOptions{Both: true})
	if err != nil {
		return UnitContext{}, err
	}
	legend := r.legendMap(ctx) // best-effort; empty map on failure

	return selectUnitContext(resp.Incidents.Active, legend, dispatchText, referenceTime), nil
}

// legendMap returns the (cached) dispatch-status legend as UnitKey→Description. Best-effort: a
// fetch failure returns an empty map and callers render raw status codes.
func (r *Resolver) legendMap(ctx context.Context) map[string]string {
	r.legendMu.Lock()
	if r.legend != nil && time.Now().Before(r.legendExpires) {
		cached := r.legend
		r.legendMu.Unlock()
		return cached
	}
	r.legendMu.Unlock()

	callCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	resp, err := r.client.Units.Legend(callCtx, r.agencyID)
	if err != nil {
		return map[string]string{}
	}
	m := make(map[string]string, len(resp.UnitLegend))
	for _, e := range resp.UnitLegend {
		m[strings.ToUpper(e.UnitKey)] = e.Description
	}

	r.legendMu.Lock()
	r.legend = m
	r.legendExpires = time.Now().Add(legendCacheTTL)
	r.legendMu.Unlock()
	return m
}

// selectUnitContext is the pure selection logic (no I/O): score each active incident, and either
// return the best confident match or the union roster of all active units. Exported-adjacent for
// direct unit testing.
func selectUnitContext(active []pulpo.Incident, legend map[string]string, dispatchText string, referenceTime time.Time) UnitContext {
	if len(active) == 0 {
		return UnitContext{}
	}

	dispatchTokens := tokenize(dispatchText)

	bestIdx, bestScore, bestOverlap := -1, 0.0, 0
	for i, inc := range active {
		score, overlap := scoreIncident(inc, dispatchTokens, referenceTime)
		if score > bestScore {
			bestScore, bestIdx, bestOverlap = score, i, overlap
		}
	}

	// A confident single-incident match requires BOTH a score over threshold AND at least one
	// shared location token — call-type + recency alone can't win, so we don't grab the wrong
	// simultaneous rescue when the dispatch clearly names a different place.
	if bestIdx >= 0 && bestScore >= matchScoreThresh && bestOverlap >= 1 {
		inc := active[bestIdx]
		return UnitContext{
			Matched:    true,
			IncidentID: inc.ID,
			CallType:   inc.CallType,
			Address:    firstNonEmpty(inc.FullDisplayAddress, inc.MedicalEmergencyDisplayAddress, inc.PublicLocation),
			Units:      unitInfos(inc.Unit, legend),
		}
	}

	// Fallback: union of units from active RESCUE/MEDICAL-type incidents only (deduped by
	// callsign). Restricting to rescue-like incidents keeps unrelated agency traffic (traffic
	// collisions, alarms) out of the prompt, so the model has a tight, relevant callsign set to
	// disambiguate against rather than the whole agency roster.
	seen := make(map[string]bool)
	var union []pulpo.Unit
	for _, inc := range active {
		if !matchesRescueHint(inc.CallType) {
			continue
		}
		for _, u := range inc.Unit {
			if u.UnitID == "" || seen[u.UnitID] {
				continue
			}
			seen[u.UnitID] = true
			union = append(union, u)
		}
	}
	return UnitContext{
		Matched: false,
		Units:   unitInfos(union, legend),
	}
}

// scoreIncident weights location-token overlap most heavily (the strongest correlation signal),
// with softer boosts for a rescue/medical call type and recency to the reference time. It also
// returns the raw location-token overlap count so the caller can require ≥1 for a confident match.
func scoreIncident(inc pulpo.Incident, dispatchTokens map[string]bool, referenceTime time.Time) (float64, int) {
	locTokens := tokenize(strings.Join([]string{
		inc.FullDisplayAddress, inc.MedicalEmergencyDisplayAddress, inc.PublicLocation,
	}, " "))
	overlap := 0
	for tok := range locTokens {
		if dispatchTokens[tok] {
			overlap++
		}
	}

	score := 1.5 * float64(overlap)

	if matchesRescueHint(inc.CallType) {
		score += 1.0
	}

	if t, ok := parseCADTime(inc.CallReceivedDateTime); ok {
		delta := referenceTime.Sub(t)
		if delta < 0 {
			delta = -delta
		}
		if delta <= recencyWindow {
			// Linear falloff from 1.0 (simultaneous) to 0 (at the window edge).
			score += 1.0 - float64(delta)/float64(recencyWindow)
		}
	}

	return score, overlap
}

// matchesRescueHint reports whether a CallType looks like a trail-rescue / medical incident.
func matchesRescueHint(callType string) bool {
	upper := strings.ToUpper(callType)
	for _, hint := range rescueCallTypeHints {
		if strings.Contains(upper, hint) {
			return true
		}
	}
	return false
}

// unitInfos decodes each unit's dispatch status via the legend, preserving order.
func unitInfos(units []pulpo.Unit, legend map[string]string) []UnitInfo {
	out := make([]UnitInfo, 0, len(units))
	for _, u := range units {
		if u.UnitID == "" {
			continue
		}
		status := u.DispatchStatus
		if decoded, ok := legend[strings.ToUpper(u.DispatchStatus)]; ok && decoded != "" {
			status = decoded
		}
		out = append(out, UnitInfo{ID: u.UnitID, Status: status})
	}
	return out
}

// tokenize lowercases text and returns the set of significant word tokens (alphanumeric, at least
// locationTokenMinLn long) so location comparison ignores punctuation and short filler words.
func tokenize(s string) map[string]bool {
	out := make(map[string]bool)
	for _, field := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		// A separator is any rune that is neither a lowercase letter nor a digit. Written
		// without a negated group so staticcheck (QF1001 De Morgan) stays quiet.
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	}) {
		if len(field) >= locationTokenMinLn {
			out[field] = true
		}
	}
	return out
}

// parseCADTime tries the timestamp layouts PulsePoint has been observed to emit.
func parseCADTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{
		time.RFC3339,
		"2006-01-02T15:04:05Z",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
