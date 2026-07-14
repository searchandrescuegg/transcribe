package main

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	// Embeds Go's tzdata in the binary so time.LoadLocation works on distroless-static
	// (no system /usr/share/zoneinfo). Adds ~450KB to the image; cheap insurance.
	_ "time/tzdata"

	"alpineworks.io/ootel"
	"github.com/redis/go-redis/v9"
	openai "github.com/sashabaranov/go-openai"
	anthropicClient "github.com/searchandrescuegg/transcribe/internal/anthropic"
	"github.com/searchandrescuegg/transcribe/internal/asr"
	"github.com/searchandrescuegg/transcribe/internal/calltypes"
	"github.com/searchandrescuegg/transcribe/internal/config"
	"github.com/searchandrescuegg/transcribe/internal/dataset"
	"github.com/searchandrescuegg/transcribe/internal/dragonfly"
	"github.com/searchandrescuegg/transcribe/internal/logging"
	openaiClient "github.com/searchandrescuegg/transcribe/internal/openai"
	"github.com/searchandrescuegg/transcribe/internal/pulsar"
	"github.com/searchandrescuegg/transcribe/internal/pulsepoint"
	"github.com/searchandrescuegg/transcribe/internal/s3"
	"github.com/searchandrescuegg/transcribe/internal/slackctl"
	"github.com/searchandrescuegg/transcribe/internal/transcribe"
	"github.com/sourcegraph/conc/pool"
	"go.opentelemetry.io/contrib/instrumentation/host"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
)

func main() {
	// FIX (review item #15/16): single source of truth for LOG_LEVEL. Previously main.go read
	// LOG_LEVEL directly from the env (defaulting to "error") while config.LogLevel was parsed
	// but never used. Config now loads first; the slog default is configured from c.LogLevel and
	// every downstream slog-aware client (notably the Pulsar consumer logger below) inherits it
	// without reordering the init sequence.
	c, err := config.NewConfig()
	if err != nil {
		log.Fatalf("could not create config: %s", err)
	}

	slogLevel, err := logging.LogLevelToSlogLevel(c.LogLevel)
	if err != nil {
		log.Fatalf("could not convert log level: %s", err)
	}

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slogLevel,
	})))

	// Override the process-wide time.Local so every `.Local()` call in the codebase formats
	// against the operator's preferred timezone (default: America/Los_Angeles, which handles
	// PST/PDT automatically). Empty DisplayTimezone leaves the container's TZ untouched.
	if c.DisplayTimezone != "" {
		loc, err := time.LoadLocation(c.DisplayTimezone)
		if err != nil {
			slog.Error("invalid DISPLAY_TIMEZONE; refusing to start with mis-formatted timestamps", slog.String("value", c.DisplayTimezone), slog.String("error", err.Error()))
			os.Exit(1)
		}
		time.Local = loc
		slog.Info("display timezone set", slog.String("zone", c.DisplayTimezone))
	}

	ctx := context.Background()

	exporterType := ootel.ExporterTypePrometheus
	if c.Local {
		exporterType = ootel.ExporterTypeOTLPGRPC
	}

	ootelClient := ootel.NewOotelClient(
		ootel.WithMetricConfig(
			ootel.NewMetricConfig(
				c.MetricsEnabled,
				exporterType,
				c.MetricsPort,
			),
		),
		ootel.WithTraceConfig(
			ootel.NewTraceConfig(
				c.TracingEnabled,
				c.TracingSampleRate,
				c.TracingService,
				c.TracingVersion,
			),
		),
	)

	shutdown, err := ootelClient.Init(ctx)
	if err != nil {
		slog.Error("could not create ootel client", slog.String("error", err.Error()))
		os.Exit(1)
	}

	err = runtime.Start(runtime.WithMinimumReadMemStatsInterval(5 * time.Second))
	if err != nil {
		slog.Error("could not create runtime metrics", slog.String("error", err.Error()))
		os.Exit(1)
	}

	err = host.Start()
	if err != nil {
		slog.Error("could not create host metrics", slog.String("error", err.Error()))
		os.Exit(1)
	}

	defer func() {
		_ = shutdown(ctx)
	}()

	pulsarClient, err := pulsar.NewPulsarClient(pulsar.Options{
		URL:                 c.PulsarURL,
		InputTopic:          c.PulsarInputTopic,
		Subscription:        c.PulsarSubscription,
		DLQTopic:            c.PulsarDLQTopic,
		MaxDeliveries:       c.PulsarMaxDeliveries,
		NackRedeliveryDelay: c.PulsarNackRedeliveryDelay,
	})
	if err != nil {
		slog.Error("could not create pulsar client", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer pulsarClient.Close()

	s3Client, err := s3.NewS3Client(c.S3AccessKey, c.S3SecretKey, c.S3Endpoint, c.S3Region, c.S3Bucket, c.S3Timeout)
	if err != nil {
		slog.Error("could not create s3 client", slog.String("error", err.Error()))
		os.Exit(1)
	}

	asrClient := asr.NewASRClient(c.ASREndpoint, c.ASRTimeout)

	// Confidential call-types list, decrypted with the runtime key. Empty CallTypesPath means
	// the feature is disabled — the OpenAI client falls back to its in-prompt examples and
	// does not impose a schema-level enum on call_type. Empty key with a non-empty path is a
	// configuration error: bail out loudly so it's not silently downgraded to "no constraint".
	var allowedCallTypes []string
	if c.CallTypesPath != "" {
		loaded, err := calltypes.Load(c.CallTypesPath, c.CallTypesKey)
		if err != nil {
			slog.Error("failed to load encrypted call-types file", slog.String("error", err.Error()), slog.String("path", c.CallTypesPath))
			os.Exit(1)
		}
		allowedCallTypes = loaded
		// Don't log the list itself — that defeats the point of encrypting it. Just the count.
		slog.Info("loaded confidential call-types list", slog.Int("count", len(allowedCallTypes)))
	} else {
		slog.Info("CALL_TYPES_PATH not set; running without call-type enum constraint")
	}

	// ML backend selection. Both the OpenAI-compatible path (also usable with Ollama / vLLM /
	// LiteLLM via OPENAI_BASE_URL) and the first-party Anthropic path implement
	// transcribe.MLClient (= DispatchMessageParser + RescueSummarizer); ML_BACKEND picks one
	// at startup.
	var mlClient transcribe.MLClient
	switch strings.ToLower(c.MLBackend) {
	case "anthropic":
		slog.Info("initializing Anthropic ML backend",
			slog.String("dispatch_model", c.AnthropicDispatchModel),
			slog.String("summary_model", c.AnthropicSummaryModel))
		if c.AnthropicAPIKey == "" {
			slog.Error("ML_BACKEND=anthropic requires ANTHROPIC_API_KEY")
			os.Exit(1)
		}
		mlClient = anthropicClient.NewClient(anthropicClient.Options{
			APIKey:           c.AnthropicAPIKey,
			BaseURL:          c.AnthropicBaseURL,
			DispatchModel:    c.AnthropicDispatchModel,
			SummaryModel:     c.AnthropicSummaryModel,
			CleanupModel:     c.AnthropicCleanupModel,
			AllowedCallTypes: allowedCallTypes,
			Timeout:          c.AnthropicTimeout,
			MaxTokens:        c.AnthropicMaxTokens,
		})
	case "openai":
		slog.Info("initializing OpenAI ML backend", slog.String("model", c.OpenAIModel), slog.String("base_url", c.OpenAIBaseURL))
		if c.OpenAIAPIKey == "" {
			slog.Warn("OpenAI API key not provided - this may be required depending on your endpoint configuration")
		}
		openaiConfig := openai.DefaultConfig(c.OpenAIAPIKey)
		if c.OpenAIBaseURL != "https://api.openai.com/v1" {
			openaiConfig.BaseURL = c.OpenAIBaseURL
		}
		openaiConfig.HTTPClient = &http.Client{Timeout: c.OpenAITimeout}
		mlClient = openaiClient.NewOpenAIClient(
			openai.NewClientWithConfig(openaiConfig),
			c.OpenAIModel,
			allowedCallTypes,
			c.OpenAIEnableThinking,
		)
	default:
		slog.Error("unknown ML_BACKEND; expected \"openai\" or \"anthropic\"", slog.String("value", c.MLBackend))
		os.Exit(1)
	}

	// Optional dataset capture: records raw transcriptions + LLM interactions to Postgres for
	// offline prompt refinement. Best-effort (drops rather than blocks) and fully disabled
	// unless DATASET_ENABLED=true. When enabled, the MLClient is wrapped in a recording
	// decorator so every dispatch-parse / rescue-summary call is logged with its I/O.
	var recorder dataset.Recorder
	if c.DatasetEnabled {
		if c.DatasetPostgresURL == "" {
			slog.Error("DATASET_ENABLED=true requires DATASET_POSTGRES_URL")
			os.Exit(1)
		}
		store, err := dataset.NewStore(ctx, c.DatasetPostgresURL, c.DatasetBufferSize)
		if err != nil {
			slog.Error("could not initialize dataset store", slog.String("error", err.Error()))
			os.Exit(1)
		}
		defer func() {
			_ = store.Close()
		}()
		recorder = store

		dispatchModel, summaryModel, cleanupModel := c.OpenAIModel, c.OpenAIModel, c.OpenAIModel
		if strings.ToLower(c.MLBackend) == "anthropic" {
			dispatchModel, summaryModel, cleanupModel = c.AnthropicDispatchModel, c.AnthropicSummaryModel, c.AnthropicCleanupModel
		}
		mlClient = dataset.NewRecordingMLClient(mlClient, store, dataset.DecoratorOptions{
			Backend:          strings.ToLower(c.MLBackend),
			DispatchModel:    dispatchModel,
			SummaryModel:     summaryModel,
			CleanupModel:     cleanupModel,
			AllowedCallTypes: allowedCallTypes,
		})
		slog.Info("dataset capture enabled")
	}

	dragonflyClient, err := dragonfly.NewClient(ctx, c.DragonflyRequestTimeout, &redis.Options{
		Addr:     c.DragonflyAddress,
		Password: c.DragonflyPassword,
		DB:       c.DragonflyDB,
	})
	if err != nil {
		slog.Error("failed to create dragonfly client", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer func() {
		_ = dragonflyClient.Close()
	}()

	// Optional CAD (PulsePoint) unit enrichment: resolves the units assigned to the active
	// rescue so garbled unit callsigns can be canonicalized in cleanup + summaries. Best-effort
	// and fully disabled unless PULPO_ENABLED=true. A nil resolver means "no enrichment".
	var unitResolver transcribe.UnitResolver
	if c.PulpoEnabled {
		if c.PulpoBaseURL == "" || c.PulpoAPIKey == "" || c.PulpoAgencyID == "" {
			slog.Error("PULPO_ENABLED=true requires PULPO_BASE_URL, PULPO_API_KEY, and PULPO_AGENCY_ID")
			os.Exit(1)
		}
		unitResolver = pulsepoint.NewResolver(pulsepoint.Options{
			BaseURL:  c.PulpoBaseURL,
			APIKey:   c.PulpoAPIKey,
			AgencyID: c.PulpoAgencyID,
			Username: c.PulpoUsername,
			Password: c.PulpoPassword,
			Timeout:  c.PulpoTimeout,
		})
		slog.Info("PulsePoint unit enrichment enabled", slog.String("agency_id", c.PulpoAgencyID))
	}

	transcribeClient := transcribe.NewTranscribeClient(c, pulsarClient, s3Client, asrClient, mlClient, dragonflyClient, recorder, unitResolver)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	slog.Info("starting transcribe service", slog.Int("workers", c.WorkerCount))

	workerPool := pool.New()

	processingCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	for i := 0; i < c.WorkerCount; i++ {
		workerPool.Go(func() {
			transcribeClient.Work(processingCtx)
		})
	}

	// FIX (review item #10 / option B): start the durable TAC-expiry sweeper alongside the
	// worker pool. Replaces the in-process time.AfterFunc that lost scheduled "channel closed"
	// Slack messages on every restart.
	workerPool.Go(func() {
		transcribeClient.Sweep(processingCtx)
	})

	// Slack interactivity controller (Cancel / Extend buttons). Optional: when SLACK_APP_TOKEN
	// is unset the feature is silently disabled. When set, the controller opens an outbound
	// Socket Mode WebSocket to Slack — no public HTTP endpoint required.
	slackController, err := slackctl.New(c, dragonflyClient)
	switch {
	case errors.Is(err, slackctl.ErrSocketModeDisabled):
		slog.Info("Slack interactivity disabled (SLACK_APP_TOKEN not set)")
	case err != nil:
		slog.Error("could not create Slack controller", slog.String("error", err.Error()))
		os.Exit(1)
	default:
		workerPool.Go(func() {
			if err := slackController.Run(processingCtx); err != nil && !errors.Is(err, context.Canceled) {
				slog.Error("Slack controller exited with error", slog.String("error", err.Error()))
			}
		})
	}

	<-sigChan
	slog.Info("received shutdown signal, stopping workers")
	cancel()
	workerPool.Wait()
	slog.Info("all workers stopped")
}
