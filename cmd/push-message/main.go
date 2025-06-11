package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
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
		fmt.Println("Usage: go run main.go -message <message> [-url <pulsar-url>] [-topic <topic>]")
		fmt.Println("Example: go run main.go -message 'audio_file.wav'")
		os.Exit(1)
	}

	// Create Pulsar client
	client, err := pulsar.NewClient(pulsar.ClientOptions{
		URL: *pulsarURL,
	})
	if err != nil {
		log.Fatalf("Failed to create Pulsar client: %v", err)
	}
	defer client.Close()

	// Create producer
	producer, err := client.CreateProducer(pulsar.ProducerOptions{
		Topic: *topic,
	})
	if err != nil {
		log.Fatalf("Failed to create producer: %v", err)
	}
	defer producer.Close()

	// Create JSON payload
	msg := FileMessage{
		File: *message,
	}
	
	jsonPayload, err := json.Marshal(msg)
	if err != nil {
		log.Fatalf("Failed to marshal JSON: %v", err)
	}

	// Send message
	ctx := context.Background()
	_, err = producer.Send(ctx, &pulsar.ProducerMessage{
		Payload: jsonPayload,
	})
	if err != nil {
		log.Fatalf("Failed to send message: %v", err)
	}

	fmt.Printf("Successfully sent JSON message: %s\n", string(jsonPayload))
}