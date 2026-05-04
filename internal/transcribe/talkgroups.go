package transcribe

// FIX (review item #22): the "1399" literal previously appeared in three call sites
// (transcribe.go, rules.go, slack.go) and the OpenMHz URL builder. Promoted to a single
// constant so the dispatch-channel identity is sourced in one place; the talkgroup maps
// below remain the source of truth for everything else NORCOM-specific.
const FireDispatch1TGID = "1399"

type TalkgroupInformation struct {
	TGID      string `json:"tg_id"`
	FullName  string `json:"full_name"`
	ShortName string `json:"short_name"`
	// RadioShortCode is what the dispatch transcript / ML output uses to refer to the channel
	// (e.g. "TAC1"), distinct from ShortName which is the human-friendly label ("FTAC 1").
	RadioShortCode string `json:"radio_short_code"`
}

// FIX (review item #21): talkgroupFromTGID is the single source of truth. The short-code map
// below is derived in init() so adding a new TAC means editing only one place. Previously the
// two maps were maintained in parallel and could silently drift.
var talkgroupFromTGID = map[string]TalkgroupInformation{
	"1399": {TGID: "1399", FullName: "NORCOM - Fire Dispatch 1", ShortName: "FDisp 1", RadioShortCode: "FDisp 1"},
	"1389": {TGID: "1389", FullName: "NORCOM - Fire Tactical 1", ShortName: "FTAC 1", RadioShortCode: "TAC1"},
	"1387": {TGID: "1387", FullName: "NORCOM - Fire Tactical 2", ShortName: "FTAC 2", RadioShortCode: "TAC2"},
	"1385": {TGID: "1385", FullName: "NORCOM - Fire Tactical 3", ShortName: "FTAC 3", RadioShortCode: "TAC3"},
	"1383": {TGID: "1383", FullName: "NORCOM - Fire Tactical 4", ShortName: "FTAC 4", RadioShortCode: "TAC4"},
	"1381": {TGID: "1381", FullName: "NORCOM - Fire Tactical 5", ShortName: "FTAC 5", RadioShortCode: "TAC5"},
	"1379": {TGID: "1379", FullName: "NORCOM - Fire Tactical 6", ShortName: "FTAC 6", RadioShortCode: "TAC6"},
	"1377": {TGID: "1377", FullName: "NORCOM - Fire Tactical 7", ShortName: "FTAC 7", RadioShortCode: "TAC7"},
	"1963": {TGID: "1963", FullName: "NORCOM - Fire Tactical 8", ShortName: "FTAC 8", RadioShortCode: "TAC8"},
	"1965": {TGID: "1965", FullName: "NORCOM - Fire Tactical 9", ShortName: "FTAC 9", RadioShortCode: "TAC9"},
	"1967": {TGID: "1967", FullName: "NORCOM - Fire Tactical 10", ShortName: "FTAC 10", RadioShortCode: "TAC10"},
}

var talkgroupFromRadioShortCode map[string]TalkgroupInformation

func init() {
	talkgroupFromRadioShortCode = make(map[string]TalkgroupInformation, len(talkgroupFromTGID))
	for _, tg := range talkgroupFromTGID {
		talkgroupFromRadioShortCode[tg.RadioShortCode] = tg
	}
}

// TalkgroupByTGID looks up the canonical talkgroup record by its TGID. Exported so the
// slackctl package can resolve short codes (TAC1, TAC10, ...) when handling Switch-TAC
// actions, without slackctl having to import the unexported maps.
func TalkgroupByTGID(tgid string) (TalkgroupInformation, bool) {
	tg, ok := talkgroupFromTGID[tgid]
	return tg, ok
}
