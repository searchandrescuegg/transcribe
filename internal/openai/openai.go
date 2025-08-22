package openai

import (
	"context"
	"encoding/json"
	"fmt"

	openai "github.com/sashabaranov/go-openai"
	"github.com/sashabaranov/go-openai/jsonschema"
	"github.com/searchandrescuegg/transcribe/internal/ml"
)

type OpenAIClient struct {
	client *openai.Client
	model  string
}

func NewOpenAIClient(client *openai.Client, model string) *OpenAIClient {
	return &OpenAIClient{
		client: client,
		model:  model,
	}
}

func (oc *OpenAIClient) ParseRelevantInformationFromDispatchMessage(transcription string) (*ml.DispatchMessage, error) {
	dmSchema, err := generateDispatchMessageSchema()
	if err != nil {
		return nil, fmt.Errorf("failed to generate JSON schema: %w", err)
	}

	resp, err := oc.client.CreateChatCompletion(
		context.Background(),
		openai.ChatCompletionRequest{
			Model: oc.model,
			Messages: []openai.ChatCompletionMessage{
				{
					Role: openai.ChatMessageRoleSystem,
					Content: `You are a tool to accurately parse relevant information from a transcription of Fire Department radio messages.
You will need to extract the call type and the tactical channel (TAC) from the transcription, including the FULL transcription.
Please return the information in the defined format.
Call types can include "Aid Emergency", "MVC", "MVC Aid Emergency", "AFA Commercial", "Rescue - Trail", etc.
If the call type can not be determined, return "Unknown".
The tactical channel (TAC) should be in the format "TAC1", "TAC2", etc. Do not include a space between "TAC" and the number. If it appears as SPFR Repeater, assume it is "TAC8".
Please clean the transcription to update any misspellings, incorrect locations, and generally ensure that it is clear and concise.
Do not add any additional information or context that is not present in the transcription.`,
				},
				{
					Role:    openai.ChatMessageRoleUser,
					Content: transcription,
				},
			},
			ResponseFormat: &openai.ChatCompletionResponseFormat{
				Type: openai.ChatCompletionResponseFormatTypeJSONSchema,
				JSONSchema: &openai.ChatCompletionResponseFormatJSONSchema{
					Name:   "dispatch_message",
					Schema: dmSchema,
					Strict: true,
				},
			},
		},
	)

	if err != nil {
		return nil, fmt.Errorf("chat completion error: %w", err)
	}

	var dispatchMessage ml.DispatchMessage
	if err := json.Unmarshal([]byte(resp.Choices[0].Message.Content), &dispatchMessage); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	dispatchMessage.Transcription = transcription
	return &dispatchMessage, nil
}

func generateDispatchMessageSchema() (*jsonschema.Definition, error) {
	var dm ml.DispatchMessage
	return jsonschema.GenerateSchemaForType(&dm)
}
