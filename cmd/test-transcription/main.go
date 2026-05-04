package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/sashabaranov/go-openai"
	"github.com/searchandrescuegg/transcribe/internal/calltypes"
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

	// Optional: same encrypted call-types file as the production binary, so local iteration
	// exercises the same prompt + schema enum as prod.
	var allowedCallTypes []string
	if path := os.Getenv("CALL_TYPES_PATH"); path != "" {
		loaded, err := calltypes.Load(path, os.Getenv("CALL_TYPES_KEY"))
		if err != nil {
			slog.Error("failed to load call-types file", slog.String("error", err.Error()))
			os.Exit(1)
		}
		allowedCallTypes = loaded
	}

	openaiConfig := openai.DefaultConfig(apiKey)
	openaiConfig.BaseURL = baseURL
	openaiConfig.HTTPClient = &http.Client{Timeout: time.Second * 30}
	// Mirror prod's default — thinking off for fast, deterministic responses.
	mlClient := openaiClient.NewOpenAIClient(
		openai.NewClientWithConfig(openaiConfig),
		modelName,
		allowedCallTypes,
		false,
	)

	transcriptionBytes, err := os.ReadFile(transcriptionFile)
	if err != nil {
		slog.Error("could not read transcription file", slog.String("error", err.Error()))
		os.Exit(1)
	}

	transcription := string(transcriptionBytes)

	message, err := mlClient.ParseRelevantInformationFromDispatchMessage(context.Background(), transcription)
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
