package ollama

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	ollama "github.com/ollama/ollama/api"
)

type OllamaClient struct {
	client *ollama.Client
}

func NewOllamaClient(baseUrl *url.URL, httpClient *http.Client) (*OllamaClient, error) {
	return &OllamaClient{client: ollama.NewClient(baseUrl, httpClient)}, nil
}

type DispatchMessage struct {
	CallType      string `json:"call_type"`
	TACChannel    string `json:"tac_channel"`
	Transcription string `json:"transcription"`
}

var DispatchMessageResponseFormat = json.RawMessage(`{
  "type": "object",
  "properties": {
    "call_type": {
      "type": "string"
    },
	"tac_channel": {
      "type": "string"
    }
  },
  "required": [
    "call_type",
    "tac_channel"
  ]
}`)

func (oc *OllamaClient) ParseRelevantInformationFromDispatchMessage(transcription string) (*DispatchMessage, error) {
	messages := []ollama.Message{
		{
			Role: "system",
			Content: `You are a tool to accurately parse relevant information from a transcription of Fire Department radio messages.
			You will need to extract the call type and the tactical channel (TAC) from the transcription.
			Please return the information in the JSON format defined below.
			Call types can include "Aid Emergency", "MVC", "MVC Aid Emergency", "AFA Commercial", "Rescue - Trail", etc.
			The tactical channel (TAC) should be in the format "TAC1", "TAC2", etc. Do not include a space between "TAC" and the number.
			Do not add any additional information or context that is not present in the transcription.
			`,
		},
		{
			Role:    "user",
			Content: transcription,
		},
	}

	ctx := context.Background()
	req := &ollama.ChatRequest{
		Model:    "llama3.1:8b",
		Messages: messages,
		Format:   DispatchMessageResponseFormat,
		Stream:   func(b bool) *bool { return &b }(false),
	}

	var result *DispatchMessage
	respFunc := func(resp ollama.ChatResponse) error {
		if !resp.Done {
			return nil // Continue processing until the response is complete
		}
		var dispatchMessageResponse DispatchMessage

		if err := json.Unmarshal([]byte(resp.Message.Content), &dispatchMessageResponse); err != nil {
			return fmt.Errorf("failed to unmarshal transcription response: %w", err)
		}
		dispatchMessageResponse.Transcription = transcription // Include the original transcription in the response
		result = &dispatchMessageResponse
		return nil
	}

	err := oc.client.Chat(ctx, req, respFunc)
	if err != nil {
		return nil, err
	}

	if result == nil {
		return nil, fmt.Errorf("no response received from Ollama")
	}

	return result, nil
}
