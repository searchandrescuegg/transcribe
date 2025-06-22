package transcribe

type TalkgroupInformation struct {
	TGID      string `json:"tg_id"`
	FullName  string `json:"full_name"`
	ShortName string `json:"short_name"`
}

var talkgroupFromTGID = map[string]TalkgroupInformation{
	"1399": {TGID: "1399", FullName: "NORCOM - Fire Dispatch 1", ShortName: "FDisp 1"},
	"1389": {TGID: "1389", FullName: "NORCOM - Fire Tactical 1", ShortName: "FTAC 1"},
	"1387": {TGID: "1387", FullName: "NORCOM - Fire Tactical 2", ShortName: "FTAC 2"},
	"1385": {TGID: "1385", FullName: "NORCOM - Fire Tactical 3", ShortName: "FTAC 3"},
	"1383": {TGID: "1383", FullName: "NORCOM - Fire Tactical 4", ShortName: "FTAC 4"},
	"1381": {TGID: "1381", FullName: "NORCOM - Fire Tactical 5", ShortName: "FTAC 5"},
	"1379": {TGID: "1379", FullName: "NORCOM - Fire Tactical 6", ShortName: "FTAC 6"},
	"1377": {TGID: "1377", FullName: "NORCOM - Fire Tactical 7", ShortName: "FTAC 7"},
	"1963": {TGID: "1963", FullName: "NORCOM - Fire Tactical 8", ShortName: "FTAC 8"},
	"1965": {TGID: "1965", FullName: "NORCOM - Fire Tactical 9", ShortName: "FTAC 9"},
	"1967": {TGID: "1967", FullName: "NORCOM - Fire Tactical 10", ShortName: "FTAC 10"},
}

var talkgroupFromRadioShortCode = map[string]TalkgroupInformation{
	"FDisp 1": {TGID: "1399", FullName: "NORCOM - Fire Dispatch 1", ShortName: "FDisp 1"},
	"TAC1":    {TGID: "1389", FullName: "NORCOM - Fire Tactical 1", ShortName: "FTAC 1"},
	"TAC2":    {TGID: "1387", FullName: "NORCOM - Fire Tactical 2", ShortName: "FTAC 2"},
	"TAC3":    {TGID: "1385", FullName: "NORCOM - Fire Tactical 3", ShortName: "FTAC 3"},
	"TAC4":    {TGID: "1383", FullName: "NORCOM - Fire Tactical 4", ShortName: "FTAC 4"},
	"TAC5":    {TGID: "1381", FullName: "NORCOM - Fire Tactical 5", ShortName: "FTAC 5"},
	"TAC6":    {TGID: "1379", FullName: "NORCOM - Fire Tactical 6", ShortName: "FTAC 6"},
	"TAC7":    {TGID: "1377", FullName: "NORCOM - Fire Tactical 7", ShortName: "FTAC 7"},
	"TAC8":    {TGID: "1963", FullName: "NORCOM - Fire Tactical 8", ShortName: "FTAC 8"},
	"TAC9":    {TGID: "1965", FullName: "NORCOM - Fire Tactical 9", ShortName: "FTAC 9"},
	"TAC10":   {TGID: "1967", FullName: "NORCOM - Fire Tactical 10", ShortName: "FTAC 10"},
}
