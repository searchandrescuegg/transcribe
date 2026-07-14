// Command test-summary is a prompt-iteration tool for the rescue interpretation feature.
// It reads a JSON file describing a rescue (dispatch transcript + ordered TAC transmissions),
// calls the configured ML backend, and prints the structured RescueSummary back to stdout.
// Use it to iterate on the summary prompt + King County gazetteer in internal/prompts without
// having to round-trip through Pulsar / Slack / docker-compose.
//
// Backend selection mirrors the service (ML_BACKEND=openai|anthropic):
//
//	# OpenAI-compatible (Ollama / vLLM / OpenAI):
//	OPENAI_API_KEY=... OPENAI_BASE_URL=... OPENAI_MODEL_NAME=... \
//	go run ./cmd/test-summary -input data/rescue.json
//
//	# Anthropic (Claude):
//	ML_BACKEND=anthropic ANTHROPIC_API_KEY=... ANTHROPIC_MODEL=claude-sonnet-5 \
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

// inputDoc mirrors the JSON file format. tac entries map directly onto ml.TACTranscript.
//
// previous_summary and unit_context are optional: set previous_summary to eyeball the ADDITIVE
// path (the model should extend it rather than rewrite), and unit_context to a rendered CAD unit
// block to see unit-callsign canonicalization.
type inputDoc struct {
	Dispatch         string             `json:"dispatch"`
	DispatchCallType string             `json:"dispatch_call_type"`
	TACChannel       string             `json:"tac_channel"`
	TAC              []ml.TACTranscript `json:"tac"`
	PreviousSummary  *ml.RescueSummary  `json:"previous_summary,omitempty"`
	UnitContext      string             `json:"unit_context,omitempty"`
}

// summarizerCleaner is the subset of the ML backend this CLI drives — both concrete backends
// implement it, so we can also exercise the per-transmission cleanup path via -clean.
type summarizerCleaner interface {
	ml.RescueSummarizer
	ml.TranscriptCleaner
}

func main() {
	var (
		inputPath = flag.String("input", "", "Path to the rescue JSON file (required)")
		timeout   = flag.Duration("timeout", 120*time.Second, "Request timeout")
		clean     = flag.String("clean", "", "Instead of summarizing, run the per-transmission cleanup on this raw text (uses -input's dispatch as context and its unit_context, both optional)")
	)
	flag.Parse()

	if *inputPath == "" && *clean == "" {
		fmt.Fprintln(os.Stderr, "-input is required (unless -clean is used)")
		flag.Usage()
		os.Exit(2)
	}

	var doc inputDoc
	if *inputPath != "" {
		rawInput, err := os.ReadFile(*inputPath)
		if err != nil {
			slog.Error("could not read input file", slog.String("path", *inputPath), slog.String("error", err.Error()))
			os.Exit(1)
		}
		if err := json.Unmarshal(rawInput, &doc); err != nil {
			slog.Error("could not parse input JSON", slog.String("error", err.Error()))
			os.Exit(1)
		}
	}
	if *clean == "" && doc.Dispatch == "" && len(doc.TAC) == 0 {
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

	var client summarizerCleaner
	switch backend := strings.ToLower(getenv("ML_BACKEND", "openai")); backend {
	case "anthropic":
		apiKey := os.Getenv("ANTHROPIC_API_KEY")
		if apiKey == "" {
			slog.Error("ML_BACKEND=anthropic requires ANTHROPIC_API_KEY")
			os.Exit(1)
		}
		model := getenv("ANTHROPIC_MODEL", "claude-sonnet-5")
		// For -clean iteration, ANTHROPIC_CLEANUP_MODEL overrides (defaults to ANTHROPIC_MODEL) so
		// you can A/B a stronger cleanup model without touching the summary model.
		cleanupModel := getenv("ANTHROPIC_CLEANUP_MODEL", model)
		client = anthropicClient.NewClient(anthropicClient.Options{
			APIKey:           apiKey,
			BaseURL:          os.Getenv("ANTHROPIC_BASE_URL"),
			DispatchModel:    model,
			SummaryModel:     model,
			CleanupModel:     cleanupModel,
			AllowedCallTypes: allowedCallTypes,
			Timeout:          *timeout,
			MaxTokens:        2048,
		})
		slog.Info("using Anthropic backend", slog.String("model", model), slog.String("cleanup_model", cleanupModel))
	case "openai":
		modelName := os.Getenv("OPENAI_MODEL_NAME")
		if modelName == "" {
			slog.Error("OPENAI_MODEL_NAME environment variable is required")
			os.Exit(1)
		}
		cfg := openai.DefaultConfig(os.Getenv("OPENAI_API_KEY"))
		cfg.BaseURL = getenv("OPENAI_BASE_URL", "https://api.openai.com/v1")
		cfg.HTTPClient = &http.Client{Timeout: *timeout}
		client = openaiClient.NewOpenAIClient(openai.NewClientWithConfig(cfg), modelName, allowedCallTypes, false)
		slog.Info("using OpenAI backend", slog.String("model", modelName))
	default:
		slog.Error("unknown ML_BACKEND; expected \"openai\" or \"anthropic\"", slog.String("value", backend))
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	// Cleanup mode: run the per-transmission cleaner on the raw -clean text and print the result.
	if *clean != "" {
		res, err := client.CleanTACTranscript(ctx, ml.TACCleanupInput{
			Text:            *clean,
			DispatchContext: doc.Dispatch,
			UnitContext:     doc.UnitContext,
		})
		if err != nil {
			slog.Error("tac cleanup failed", slog.String("error", err.Error()))
			os.Exit(1)
		}
		fmt.Println(res.CleanedText)
		return
	}

	summary, err := client.SummarizeRescue(ctx, ml.RescueSummaryInput{
		DispatchTranscription: doc.Dispatch,
		DispatchCallType:      doc.DispatchCallType,
		TACChannel:            doc.TACChannel,
		TACTranscripts:        doc.TAC,
		PreviousSummary:       doc.PreviousSummary,
		UnitContext:           doc.UnitContext,
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
