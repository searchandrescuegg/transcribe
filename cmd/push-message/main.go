package main

import (
	"context"
	"encoding/json"
	"flag"
	"log/slog"
	"os"

	"github.com/apache/pulsar-client-go/pulsar"
)

type FileMessage struct {
	File string `json:"file"`
}

func main() {
	var (
		pulsarURL = flag.String("url", "pulsar://localhost:6650", "Pulsar service URL")
		topic     = flag.String("topic", "public/transcribe/file-queue", "Topic to send message to")
		message   = flag.String("message", "", "Message to send")
	)
	flag.Parse()

	if *message == "" {
		slog.Info("Usage: go run main.go -message <message> [-url <pulsar-url>] [-topic <topic>]")
		slog.Info("Example: go run main.go -message 'audio_file.wav'")
		os.Exit(1)
	}

	// Create Pulsar client
	client, err := pulsar.NewClient(pulsar.ClientOptions{
		URL: *pulsarURL,
	})
	if err != nil {
		slog.Error("failed to create Pulsar client", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer client.Close()

	// Create producer
	producer, err := client.CreateProducer(pulsar.ProducerOptions{
		Topic: *topic,
	})
	if err != nil {
		slog.Error("failed to create producer", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer producer.Close()

	// Create JSON payload
	msg := FileMessage{
		File: *message,
	}

	jsonPayload, err := json.Marshal(msg)
	if err != nil {
		slog.Error("failed to marshal JSON", slog.String("error", err.Error()))
		os.Exit(1)
	}

	// Send message
	ctx := context.Background()
	_, err = producer.Send(ctx, &pulsar.ProducerMessage{
		Payload: jsonPayload,
	})
	if err != nil {
		slog.Error("failed to send message", slog.String("error", err.Error()))
		os.Exit(1)
	}

	slog.Info("successfully sent JSON message", slog.String("payload", string(jsonPayload)))
}
