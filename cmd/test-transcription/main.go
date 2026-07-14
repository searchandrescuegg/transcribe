// Command test-transcription is a prompt-iteration tool for the dispatch parser. It reads a
// transcription from TRANSCRIPTION_FILE, runs it through the configured ML backend, and prints
// the structured dispatch messages. Use it to iterate on the dispatch prompt + King County
// gazetteer in internal/prompts without round-tripping through Pulsar / Slack / docker-compose.
//
// Backend selection mirrors the service (ML_BACKEND=openai|anthropic):
//
//	# OpenAI-compatible (Ollama / vLLM / OpenAI):
//	OPENAI_API_KEY=... OPENAI_BASE_URL=... OPENAI_MODEL_NAME=... \
//	TRANSCRIPTION_FILE=data/transcription.txt go run ./cmd/test-transcription
//
//	# Anthropic (Claude):
//	ML_BACKEND=anthropic ANTHROPIC_API_KEY=... ANTHROPIC_MODEL=claude-haiku-4-5 \
//	TRANSCRIPTION_FILE=data/transcription.txt go run ./cmd/test-transcription
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/sashabaranov/go-openai"
	anthropicClient "github.com/searchandrescuegg/transcribe/internal/anthropic"
	"github.com/searchandrescuegg/transcribe/internal/calltypes"
	"github.com/searchandrescuegg/transcribe/internal/ml"
	openaiClient "github.com/searchandrescuegg/transcribe/internal/openai"
)

func getenv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func main() {
	transcriptionFile, exists := os.LookupEnv("TRANSCRIPTION_FILE")
	if !exists {
		slog.Error("TRANSCRIPTION_FILE environment variable is required")
		os.Exit(1)
	}

	// Optional: same encrypted call-types file as the production binary, so local iteration
	// exercises the same prompt + schema enum (and the baked-in King County gazetteer) as prod.
	var allowedCallTypes []string
	if path := os.Getenv("CALL_TYPES_PATH"); path != "" {
		loaded, err := calltypes.Load(path, os.Getenv("CALL_TYPES_KEY"))
		if err != nil {
			slog.Error("failed to load call-types file", slog.String("error", err.Error()))
			os.Exit(1)
		}
		allowedCallTypes = loaded
	}

	var parser ml.DispatchMessageParser
	switch backend := strings.ToLower(getenv("ML_BACKEND", "openai")); backend {
	case "anthropic":
		apiKey := os.Getenv("ANTHROPIC_API_KEY")
		if apiKey == "" {
			slog.Error("ML_BACKEND=anthropic requires ANTHROPIC_API_KEY")
			os.Exit(1)
		}
		model := getenv("ANTHROPIC_MODEL", "claude-haiku-4-5")
		parser = anthropicClient.NewClient(anthropicClient.Options{
			APIKey:           apiKey,
			BaseURL:          os.Getenv("ANTHROPIC_BASE_URL"),
			DispatchModel:    model,
			SummaryModel:     model,
			AllowedCallTypes: allowedCallTypes,
			Timeout:          30 * time.Second,
			MaxTokens:        2048,
		})
		slog.Info("using Anthropic backend", slog.String("model", model))
	case "openai":
		modelName, ok := os.LookupEnv("OPENAI_MODEL_NAME")
		if !ok {
			slog.Error("OPENAI_MODEL_NAME environment variable is required")
			os.Exit(1)
		}
		cfg := openai.DefaultConfig(os.Getenv("OPENAI_API_KEY"))
		cfg.BaseURL = getenv("OPENAI_BASE_URL", "https://api.openai.com/v1")
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
		// Mirror prod's default — thinking off for fast, deterministic responses.
		parser = openaiClient.NewOpenAIClient(openai.NewClientWithConfig(cfg), modelName, allowedCallTypes, false)
		slog.Info("using OpenAI backend", slog.String("model", modelName))
	default:
		slog.Error("unknown ML_BACKEND; expected \"openai\" or \"anthropic\"", slog.String("value", backend))
		os.Exit(1)
	}

	transcriptionBytes, err := os.ReadFile(transcriptionFile)
	if err != nil {
		slog.Error("could not read transcription file", slog.String("error", err.Error()))
		os.Exit(1)
	}

	message, err := parser.ParseRelevantInformationFromDispatchMessage(context.Background(), string(transcriptionBytes))
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
