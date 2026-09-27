package transcribe

import (
	"encoding/json"
	"fmt"
	"time"
)

// Legacy entries (pre-feature JSON) must parse as radio and keep list indexes aligned with
// the Dragonfly list so LSET hits the right element; unparseable entries keep their slot.
func (s *DispatchSuite) TestTranscriptEntries_LegacyAndNewMixed_IndexAligned() {
	tc := s.newClientUnderTest(new(mockSlackPoster), new(mockMLClient))
	tgid := talkgroupFromRadioShortCode["TAC10"].TGID
	key := fmt.Sprintf(tacTranscriptsKeyFmt, tgid)

	s.Require().NoError(s.rdb.RPush(s.ctx, key, `{"captured_at":"11:00:00","text":"legacy"}`).Err())
	s.Require().NoError(s.rdb.RPush(s.ctx, key, `not json`).Err())
	s.Require().NoError(tc.appendEntry(s.ctx, tgid, liveTranscriptEntry{
		CapturedAt: "11:01:00", Text: "new", Kind: entryKindRadio, SlackTS: "ts-3",
	}))

	entries, err := tc.readEntries(s.ctx, tgid)
	s.Require().NoError(err)
	s.Require().Len(entries, 3)
	s.Equal(entryKindRadio, entries[0].kind(), "missing kind defaults to radio")
	s.Equal(entryKindInvalid, entries[1].kind(), "garbage keeps its slot")

	idx, e, found, err := tc.findEntryBySlackTS(s.ctx, tgid, "ts-3", entryKindRadio)
	s.Require().NoError(err)
	s.True(found)
	s.EqualValues(2, idx)

	e.Corrected = "fixed"
	e.Correction = &TranscriptCorrection{By: "U1", At: time.Unix(0, 0).UTC(), Original: "new"}
	s.Require().NoError(tc.setEntry(s.ctx, tgid, idx, e))

	raw, err := s.rdb.LIndex(s.ctx, key, 2).Result()
	s.Require().NoError(err)
	var got liveTranscriptEntry
	s.Require().NoError(json.Unmarshal([]byte(raw), &got))
	s.Equal("fixed", got.effectiveText())

	ttl, err := s.rdb.TTL(s.ctx, key).Result()
	s.Require().NoError(err)
	s.Greater(ttl, time.Duration(0), "appendEntry must stamp a TTL")
}

func (s *DispatchSuite) TestDragonfly_GetDel() {
	tc := s.newClientUnderTest(new(mockSlackPoster), new(mockMLClient))
	s.Require().NoError(s.rdb.Set(s.ctx, "k", "v", time.Minute).Err())
	v, err := tc.dragonflyClient.GetDel(s.ctx, "k")
	s.Require().NoError(err)
	s.Equal("v", v)
	v, err = tc.dragonflyClient.GetDel(s.ctx, "k")
	s.Require().NoError(err)
	s.Equal("", v, "missing key returns empty string, not an error")
}
