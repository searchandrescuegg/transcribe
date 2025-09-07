package ml

type DispatchMessages struct {
	Messages      []DispatchMessage `json:"messages"`
	Transcription string            `json:"transcription"`
}
type DispatchMessage struct {
	CallType             string `json:"call_type"`
	TACChannel           string `json:"tac_channel"`
	CleanedTranscription string `json:"cleaned_transcription"`
}

type DispatchMessageParser interface {
	ParseRelevantInformationFromDispatchMessage(transcription string) (*DispatchMessages, error)
}
