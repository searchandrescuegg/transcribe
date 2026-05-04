package main

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	// Embeds Go's tzdata in the binary so time.LoadLocation works on distroless-static
	// (no system /usr/share/zoneinfo). Adds ~450KB to the image; cheap insurance.
	_ "time/tzdata"

	"alpineworks.io/ootel"
	"github.com/redis/go-redis/v9"
	openai "github.com/sashabaranov/go-openai"
	"github.com/searchandrescuegg/transcribe/internal/asr"
	"github.com/searchandrescuegg/transcribe/internal/calltypes"
	"github.com/searchandrescuegg/transcribe/internal/config"
	"github.com/searchandrescuegg/transcribe/internal/dragonfly"
	"github.com/searchandrescuegg/transcribe/internal/logging"
	openaiClient "github.com/searchandrescuegg/transcribe/internal/openai"
	"github.com/searchandrescuegg/transcribe/internal/pulsar"
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

	// The OpenAI client is the only ML backend; Ollama (and other local servers like vLLM /
	// LiteLLM) are still usable by pointing OPENAI_BASE_URL at their /v1-compatible endpoint.
	slog.Info("initializing OpenAI ML backend", slog.String("model", c.OpenAIModel), slog.String("base_url", c.OpenAIBaseURL))
	if c.OpenAIAPIKey == "" {
		slog.Warn("OpenAI API key not provided - this may be required depending on your endpoint configuration")
	}
	openaiConfig := openai.DefaultConfig(c.OpenAIAPIKey)
	if c.OpenAIBaseURL != "https://api.openai.com/v1" {
		openaiConfig.BaseURL = c.OpenAIBaseURL
	}
	openaiConfig.HTTPClient = &http.Client{Timeout: c.OpenAITimeout}
	// Type held as transcribe.MLClient (= DispatchMessageParser + RescueSummarizer); the
	// concrete *OpenAIClient implements both and is the only MLClient we wire in prod.
	var mlClient transcribe.MLClient = openaiClient.NewOpenAIClient(
		openai.NewClientWithConfig(openaiConfig),
		c.OpenAIModel,
		allowedCallTypes,
		c.OpenAIEnableThinking,
	)

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

	transcribeClient := transcribe.NewTranscribeClient(c, pulsarClient, s3Client, asrClient, mlClient, dragonflyClient)

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
