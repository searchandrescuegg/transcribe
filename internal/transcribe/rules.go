package transcribe

import (
	"context"
	"fmt"
	"strings"

	"github.com/agnivade/levenshtein"
)

type AdornedDeconstructedKey struct {
	dk *DeconstructedKey
	ti *TalkgroupInformation
}

func (tc *TranscribeClient) IsObjectAllowed(ctx context.Context, key string) (bool, *AdornedDeconstructedKey, error) {
	var adk *AdornedDeconstructedKey

	parsedKey, err := parseKey(key)
	if err != nil {
		return false, nil, fmt.Errorf("failed to parse key: %w", err)
	}

	talkgroupInfo := talkgroupFromTGID[parsedKey.Talkgroup]

	adk = &AdornedDeconstructedKey{
		dk: parsedKey,
		ti: &talkgroupInfo,
	}

	res, err := tc.dragonflyClient.SMisMember(ctx, "allowed_talkgroups", adk.ti.TGID)
	if err != nil {
		return false, nil, fmt.Errorf("failed to check talkgroup membership: %w", err)
	}

	if len(res) != 1 {
		return false, nil, fmt.Errorf("unexpected number of results for talkgroup %s: %d", parsedKey.Talkgroup, len(res))
	}

	isAllowed := res[0]
	// Always allow fire dispatch through; we need its transcripts to detect trail rescues
	// and enable the tactical channels.
	if isAllowed || parsedKey.Talkgroup == FireDispatch1TGID {
		return true, adk, nil
	}

	// Return the parsed key on rejection too — the caller's nack-recovery path needs the
	// talkgroup to log meaningfully, and there's no information leak in giving back what's
	// already in the filename. The boolean false is the dispositive signal.
	return false, adk, nil
}

// FIX (review item #7): the prior rule (levenshtein <= 2 against "trail" only) accepted
// "tail", "rail", "trial", "frail", and any other 5-letter word within two edits, with no
// "rescue" context required. That's a false-positive risk for the only Slack alert this
// service emits. The new rule:
//   - Fast-path: literal substring match for both "trail" AND "rescue".
//   - Fuzzy fallback: tightened to distance <= 1 (covers single-character ASR typos like
//     "trails"/"fescue") AND requires both a near-trail and a near-rescue token, so a
//     stray word can't trigger an alert on its own.
func CallIsTrailRescue(calltype string) bool {
	calltype = strings.ToLower(calltype)

	if strings.Contains(calltype, "trail") && strings.Contains(calltype, "rescue") {
		return true
	}

	hasTrail, hasRescue := false, false
	for _, word := range strings.Fields(calltype) {
		if !hasTrail && levenshtein.ComputeDistance(word, "trail") <= 1 {
			hasTrail = true
		}
		if !hasRescue && levenshtein.ComputeDistance(word, "rescue") <= 1 {
			hasRescue = true
		}
		if hasTrail && hasRescue {
			return true
		}
	}
	return false
}
