package ml

type DispatchMessage struct {
	CallType             string `json:"call_type"`
	TACChannel           string `json:"tac_channel"`
	CleanedTranscription string `json:"cleaned_transcription"`
	Transcription        string `json:"transcription"`
}

type DispatchMessageParser interface {
	ParseRelevantInformationFromDispatchMessage(transcription string) (*DispatchMessage, error)
}