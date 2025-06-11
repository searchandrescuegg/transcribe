package main

import (
	"context"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"alpineworks.io/ootel"
	"github.com/searchandrescuegg/transcribe/internal/config"
	"github.com/searchandrescuegg/transcribe/internal/logging"
	"github.com/searchandrescuegg/transcribe/internal/pulsar"
	"github.com/searchandrescuegg/transcribe/internal/s3"
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
		c.PulsarOutputTopic,
		c.PulsarSubscription,
	)
	if err != nil {
		slog.Error("could not create pulsar client", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer pulsarClient.Close()

	s3Client, err := s3.NewS3Client(c.S3Region, c.S3Endpoint, c.S3Bucket, c.Local)
	if err != nil {
		slog.Error("could not create s3 client", slog.String("error", err.Error()))
		os.Exit(1)
	}

	asrClient := asr.NewASRClient(c.TargetEndpoint)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	slog.Info("starting transcribe service", slog.Int("workers", c.WorkerCount))

	workerPool := pool.New().WithMaxGoroutines(c.WorkerCount)

	processingCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	for i := 0; i < c.WorkerCount; i++ {
		workerPool.Go(func() {
			worker(processingCtx, pulsarClient, s3Client, asrClient)
		})
	}

	select {
	case <-sigChan:
		slog.Info("received shutdown signal, stopping workers")
		cancel()
		workerPool.Wait()
		slog.Info("all workers stopped")
	}
}

func worker(ctx context.Context, pulsarClient *pulsar.PulsarClient, s3Client *s3.S3Client, asrClient *asr.ASRClient) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
			fileName, err := pulsarClient.Receive(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				slog.Error("failed to receive message from pulsar", slog.String("error", err.Error()))
				time.Sleep(time.Second)
				continue
			}

			slog.Info("processing file", slog.String("filename", fileName))

			fileReader, err := s3Client.GetFile(ctx, fileName)
			if err != nil {
				slog.Error("failed to get file from s3", slog.String("filename", fileName), slog.String("error", err.Error()))
				continue
			}

			transcriptionResp, err := asrClient.Transcribe(ctx, fileName, fileReader)
			fileReader.Close()
			if err != nil {
				slog.Error("failed to transcribe file", slog.String("filename", fileName), slog.String("error", err.Error()))
				continue
			}

			err = pulsarClient.PublishTranscription(ctx, transcriptionResp.Filename, transcriptionResp.Transcription)
			if err != nil {
				slog.Error("failed to publish transcription", slog.String("filename", fileName), slog.String("error", err.Error()))
				continue
			}

			slog.Info("successfully processed file", slog.String("filename", fileName))
		}
	}
}
