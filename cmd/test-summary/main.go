// Command test-summary is a prompt-iteration tool for the rescue interpretation feature.
// It reads a JSON file describing a rescue (dispatch transcript + ordered TAC transmissions),
// calls the configured OpenAI-compatible LLM, and prints the structured RescueSummary back
// to stdout. Use it to iterate on the system prompt in internal/openai/openai.go without
// having to round-trip through Pulsar / Slack / docker-compose.
//
// Usage:
//
//	OPENAI_API_KEY=... \
//	OPENAI_BASE_URL=http://llama-cpp.intersect.k8s.lfp.rocks \
//	OPENAI_MODEL_NAME=unsloth/gemma-4-E4B-it-GGUF:UD-Q8_K_XL \
//	go run ./cmd/test-summary -input data/rescue.json
//
// Input file shape (data/rescue.json or any path you pass via -input):
//
//	{
//	  "dispatch": "Rescue Trail Tac 10 Maple Valley Battalion 381 ...",
//	  "dispatch_call_type": "Rescue - Trail",
//	  "tac_channel": "TAC10",
//	  "tac": [
//	    {"captured_at": "13:24:23", "text": "8171 to dispatch, en route ..."},
//	    {"captured_at": "13:25:50", "text": "8171 on scene, patient ambulatory ..."}
//	  ]
//	}
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/sashabaranov/go-openai"
	"github.com/searchandrescuegg/transcribe/internal/calltypes"
	"github.com/searchandrescuegg/transcribe/internal/ml"
	openaiClient "github.com/searchandrescuegg/transcribe/internal/openai"
)

// inputDoc mirrors the JSON file format. tac entries map directly onto ml.TACTranscript.
type inputDoc struct {
	Dispatch         string             `json:"dispatch"`
	DispatchCallType string             `json:"dispatch_call_type"`
	TACChannel       string             `json:"tac_channel"`
	TAC              []ml.TACTranscript `json:"tac"`
}

func main() {
	var (
		inputPath = flag.String("input", "", "Path to the rescue JSON file (required)")
		timeout   = flag.Duration("timeout", 120*time.Second, "Request timeout")
	)
	flag.Parse()

	if *inputPath == "" {
		fmt.Fprintln(os.Stderr, "-input is required")
		flag.Usage()
		os.Exit(2)
	}

	apiKey := os.Getenv("OPENAI_API_KEY")
	baseURL := os.Getenv("OPENAI_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	modelName := os.Getenv("OPENAI_MODEL_NAME")
	if modelName == "" {
		slog.Error("OPENAI_MODEL_NAME environment variable is required")
		os.Exit(1)
	}

	rawInput, err := os.ReadFile(*inputPath)
	if err != nil {
		slog.Error("could not read input file", slog.String("path", *inputPath), slog.String("error", err.Error()))
		os.Exit(1)
	}
	var doc inputDoc
	if err := json.Unmarshal(rawInput, &doc); err != nil {
		slog.Error("could not parse input JSON", slog.String("error", err.Error()))
		os.Exit(1)
	}
	if doc.Dispatch == "" && len(doc.TAC) == 0 {
		slog.Error("input must contain either dispatch text or at least one tac transmission")
		os.Exit(1)
	}

	// Optional: same encrypted call-types file as the production binary, if you want to
	// exercise the enum-constrained dispatch parser elsewhere. Not used for summary itself
	// but kept symmetric with cmd/test-transcription.
	var allowedCallTypes []string
	if path := os.Getenv("CALL_TYPES_PATH"); path != "" {
		loaded, err := calltypes.Load(path, os.Getenv("CALL_TYPES_KEY"))
		if err != nil {
			slog.Error("failed to load call-types file", slog.String("error", err.Error()))
			os.Exit(1)
		}
		allowedCallTypes = loaded
	}

	cfg := openai.DefaultConfig(apiKey)
	cfg.BaseURL = baseURL
	cfg.HTTPClient = &http.Client{Timeout: *timeout}

	client := openaiClient.NewOpenAIClient(
		openai.NewClientWithConfig(cfg),
		modelName,
		allowedCallTypes,
		false, // thinking off — fast iteration
	)

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	summary, err := client.SummarizeRescue(ctx, ml.RescueSummaryInput{
		DispatchTranscription: doc.Dispatch,
		DispatchCallType:      doc.DispatchCallType,
		TACChannel:            doc.TACChannel,
		TACTranscripts:        doc.TAC,
	})
	if err != nil {
		slog.Error("rescue summary failed", slog.String("error", err.Error()))
		os.Exit(1)
	}

	out, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		slog.Error("could not marshal summary", slog.String("error", err.Error()))
		os.Exit(1)
	}
	fmt.Println(string(out))
}
