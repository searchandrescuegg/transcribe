package transcribe

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// CAD (PulsePoint) unit-context enrichment, threaded into the per-transmission cleanup and the
// live summary so the model can canonicalize garbled unit callsigns against the units actually
// assigned to the call.
//
// The resolved, pre-rendered prompt block is cached per-rescue in Dragonfly under
// pulpo_units:<TGID> with a short TTL (PulpoRefreshInterval). That TTL means the roster
// self-refreshes as units are added over the life of the incident, and it self-expires without
// needing explicit cleanup — though it is also DEL'd on every teardown path alongside the other
// sidecars (CLAUDE.md invariant #6). A resolved-but-empty result is cached as a sentinel so a
// consistently-empty or erroring CAD feed doesn't get re-hit on every transmission.

const (
	pulpoUnitsKeyFmt = "pulpo_units:%s"

	// emptyUnitSentinel marks "resolved, but no unit context" so it can be cached distinctly
	// from a cache miss (Dragonfly Get returns "" for both a missing key and an empty value).
	emptyUnitSentinel = "\x00none"
)

// unitContextFor returns the rendered CAD unit-context block for a rescue, reading the per-rescue
// cache first and resolving+caching on a miss. Best-effort: returns "" (no context) when
// enrichment is disabled, the resolver errors, or CAD has nothing for this call — the cleanup and
// summary then run exactly as they would without enrichment.
//
// referenceTime scores incident recency; pass the rescue's dispatch capture time at dispatch, and
// time.Now() on later refreshes (active CAD incidents are inherently current, so a drifting
// reference only weakens a tiebreak, never the primary location/call-type match).
func (tc *TranscribeClient) unitContextFor(ctx context.Context, tgid, dispatchText string, referenceTime time.Time) string {
	if tc.unitResolver == nil {
		return ""
	}

	key := fmt.Sprintf(pulpoUnitsKeyFmt, tgid)
	if cached, err := tc.dragonflyClient.Get(ctx, key); err == nil && cached != "" {
		if cached == emptyUnitSentinel {
			return ""
		}
		return cached
	}

	return tc.resolveAndCacheUnitContext(ctx, tgid, dispatchText, referenceTime)
}

// resolveAndCacheUnitContext calls the resolver and writes the result (or the empty sentinel) into
// the per-rescue cache. Exposed as its own method so processDispatchCall can warm the cache at
// dispatch time without blocking the alert.
func (tc *TranscribeClient) resolveAndCacheUnitContext(ctx context.Context, tgid, dispatchText string, referenceTime time.Time) string {
	if tc.unitResolver == nil {
		return ""
	}

	block, err := tc.unitResolver.RescueUnitBlock(ctx, dispatchText, referenceTime)
	if err != nil {
		slog.Warn("unit enrichment: resolve failed; continuing without unit context",
			slog.String("error", err.Error()), slog.String("tgid", tgid))
		block = ""
	}

	toCache := block
	if toCache == "" {
		toCache = emptyUnitSentinel
	}
	if err := tc.dragonflyClient.Set(ctx, fmt.Sprintf(pulpoUnitsKeyFmt, tgid), tc.config.PulpoRefreshInterval, toCache); err != nil {
		slog.Warn("unit enrichment: failed to cache unit context",
			slog.String("error", err.Error()), slog.String("tgid", tgid))
	}
	return block
}
