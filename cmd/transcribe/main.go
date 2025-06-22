package main

import (
	"context"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"alpineworks.io/ootel"
	"github.com/redis/go-redis/v9"
	"github.com/searchandrescuegg/transcribe/internal/config"
	"github.com/searchandrescuegg/transcribe/internal/dragonfly"
	"github.com/searchandrescuegg/transcribe/internal/logging"
	"github.com/searchandrescuegg/transcribe/internal/ollama"
	"github.com/searchandrescuegg/transcribe/internal/pulsar"
	"github.com/searchandrescuegg/transcribe/internal/s3"
	"github.com/searchandrescuegg/transcribe/internal/transcribe"
	"github.com/searchandrescuegg/transcribe/pkg/asr"
	"github.com/sourcegraph/conc/pool"
	"go.opentelemetry.io/contrib/instrumentation/host"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
)

func main() {
	logLevel := os.Getenv("LOG_LEVEL")
	if logLevel == "" {
		logLevel = "error"
	}

	slogLevel, err := logging.LogLevelToSlogLevel(logLevel)
	if err != nil {
		log.Fatalf("could not convert log level: %s", err)
	}

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slogLevel,
	})))
	c, err := config.NewConfig()
	if err != nil {
		slog.Error("could not create config", slog.String("error", err.Error()))
		os.Exit(1)
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

	pulsarClient, err := pulsar.NewPulsarClient(
		c.PulsarURL,
		c.PulsarInputTopic,
		c.PulsarSubscription,
	)
	if err != nil {
		slog.Error("could not create pulsar client", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer pulsarClient.Close()

	s3Client, err := s3.NewS3Client(c.S3AccessKey, c.S3SecretKey, c.S3Endpoint, c.S3Region, c.S3Bucket)
	if err != nil {
		slog.Error("could not create s3 client", slog.String("error", err.Error()))
		os.Exit(1)
	}

	asrClient := asr.NewASRClient(c.ASREndpoint)

	ollamaClient, err := ollama.NewOllamaClient(&url.URL{Scheme: c.OllamaProtocol, Host: c.OllamaHost}, &http.Client{
		Timeout: 30 * time.Second,
	})
	if err != nil {
		slog.Error("could not create ollama client", slog.String("error", err.Error()))
		os.Exit(1)
	}

	dragonflyClient, err := dragonfly.NewClient(ctx, &redis.Options{
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

	transcribeClient := transcribe.NewTranscribeClient(c, pulsarClient, s3Client, asrClient, ollamaClient, dragonflyClient)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	slog.Info("starting transcribe service", slog.Int("workers", c.WorkerCount))

	workerPool := pool.New().WithMaxGoroutines(c.WorkerCount)

	processingCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	for i := 0; i < c.WorkerCount; i++ {
		workerPool.Go(func() {
			transcribeClient.Work(processingCtx)
		})
	}

	<-sigChan
	slog.Info("received shutdown signal, stopping workers")
	cancel()
	workerPool.Wait()
	slog.Info("all workers stopped")
}
