package transcribe

import (
	"context"
	"fmt"
	"strings"
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

	res, err := tc.dragonflyClient.SMisMember(ctx, "allowed_talkgroups", parsedKey.Talkgroup)
	if err != nil {
		return false, nil, fmt.Errorf("failed to check talkgroup membership: %w", err)
	}

	if len(res) != 1 {
		return false, nil, fmt.Errorf("unexpected number of results for talkgroup %s: %d", parsedKey.Talkgroup, len(res))
	}

	isAllowed := res[0]
	if isAllowed || parsedKey.Talkgroup == "1399" { // Always allow fire dispatch, needed to enable tactical channels later on
		return true, adk, nil
	}

	return false, nil, nil
}

// could use github.com/agnivade/levenshtein to get a more fuzzy match
func CallIsTrailRescue(calltype string) bool {
	calltype = strings.ToLower(calltype)
	if strings.Contains(calltype, "trail") && strings.Contains(calltype, "rescue") {
		return true
	}

	return false
}
