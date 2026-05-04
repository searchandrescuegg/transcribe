package transcribe

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// FIX (review item #21): guards against the case where someone hand-edits the derived
// short-code map back into existence and the two views drift again. Also pins the invariant
// that every talkgroup has a non-empty RadioShortCode and a unique one.
func TestTalkgroupShortCodeMapIsConsistent(t *testing.T) {
	assert.Equal(t, len(talkgroupFromTGID), len(talkgroupFromRadioShortCode),
		"short-code map should have the same number of entries as the TGID map")

	seenCodes := make(map[string]string, len(talkgroupFromTGID))
	for tgid, tg := range talkgroupFromTGID {
		assert.Equal(t, tgid, tg.TGID, "map key should match the entry's TGID field")
		assert.NotEmpty(t, tg.RadioShortCode, "every talkgroup must have a RadioShortCode (TGID=%s)", tgid)

		if other, dup := seenCodes[tg.RadioShortCode]; dup {
			t.Errorf("duplicate RadioShortCode %q on TGIDs %s and %s", tg.RadioShortCode, other, tgid)
		}
		seenCodes[tg.RadioShortCode] = tgid

		got, ok := talkgroupFromRadioShortCode[tg.RadioShortCode]
		assert.Truef(t, ok, "RadioShortCode %q (TGID=%s) missing from short-code map", tg.RadioShortCode, tgid)
		assert.Equalf(t, tg, got, "round-trip mismatch for RadioShortCode %q", tg.RadioShortCode)
	}
}
