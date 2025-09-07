package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/sashabaranov/go-openai"
	openaiClient "github.com/searchandrescuegg/transcribe/internal/openai"
)

func main() {
	apiKey, exists := os.LookupEnv("OPENAI_API_KEY")
	if !exists {
		apiKey = ""
	}

	baseURL, exists := os.LookupEnv("OPENAI_BASE_URL")
	if !exists {
		baseURL = "https://api.openai.com/v1"
	}

	modelName, exists := os.LookupEnv("OPENAI_MODEL_NAME")
	if !exists {
		slog.Error("OPENAI_MODEL_NAME environment variable is required")
		os.Exit(1)
	}

	transcriptionFile, exists := os.LookupEnv("TRANSCRIPTION_FILE")
	if !exists {
		slog.Error("TRANSCRIPTION_FILE environment variable is required")
		os.Exit(1)
	}

	openaiConfig := openai.DefaultConfig(apiKey)
	openaiConfig.BaseURL = baseURL
	openaiConfig.HTTPClient = &http.Client{Timeout: time.Second * 30}
	mlClient := openaiClient.NewOpenAIClient(
		openai.NewClientWithConfig(openaiConfig),
		modelName,
	)

	transcriptionBytes, err := os.ReadFile(transcriptionFile)
	if err != nil {
		slog.Error("could not read transcription file", slog.String("error", err.Error()))
		os.Exit(1)
	}

	transcription := string(transcriptionBytes)

	message, err := mlClient.ParseRelevantInformationFromDispatchMessage(transcription)
	if err != nil {
		slog.Error("could not parse relevant information from dispatch message", slog.String("error", err.Error()))
		os.Exit(1)
	}

	for _, msg := range message.Messages {
		jsonBytes, err := json.MarshalIndent(msg, "", "  ")
		if err != nil {
			slog.Error("could not marshal dispatch message to JSON", slog.String("error", err.Error()))
			os.Exit(1)
		}
		fmt.Println(string(jsonBytes))
	}
}
