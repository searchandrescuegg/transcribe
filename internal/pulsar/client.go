package pulsar

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/apache/pulsar-client-go/pulsar"
)

type PulsarClient struct {
	client   pulsar.Client
	consumer pulsar.Consumer
	producer pulsar.Producer
}

type TranscriptionMessage struct {
	Filename      string `json:"filename"`
	Transcription string `json:"transcription"`
}

func NewPulsarClient(url, inputTopic, outputTopic, subscription string) (*PulsarClient, error) {
	client, err := pulsar.NewClient(pulsar.ClientOptions{
		URL: url,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create pulsar client: %w", err)
	}

	consumer, err := client.Subscribe(pulsar.ConsumerOptions{
		Topic:            inputTopic,
		SubscriptionName: subscription,
		Type:             pulsar.Shared,
	})
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("failed to create consumer: %w", err)
	}

	producer, err := client.CreateProducer(pulsar.ProducerOptions{
		Topic: outputTopic,
	})
	if err != nil {
		consumer.Close()
		client.Close()
		return nil, fmt.Errorf("failed to create producer: %w", err)
	}

	return &PulsarClient{
		client:   client,
		consumer: consumer,
		producer: producer,
	}, nil
}

func (c *PulsarClient) Receive(ctx context.Context) (string, error) {
	msg, err := c.consumer.Receive(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to receive message: %w", err)
	}

	fileName := string(msg.Payload())
	
	err = c.consumer.Ack(msg)
	if err != nil {
		slog.Warn("failed to ack message", slog.String("error", err.Error()))
	}

	return fileName, nil
}

func (c *PulsarClient) PublishTranscription(ctx context.Context, filename, transcription string) error {
	msg := TranscriptionMessage{
		Filename:      filename,
		Transcription: transcription,
	}

	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal transcription message: %w", err)
	}

	_, err = c.producer.Send(ctx, &pulsar.ProducerMessage{
		Payload: payload,
	})
	if err != nil {
		return fmt.Errorf("failed to send transcription message: %w", err)
	}

	return nil
}

func (c *PulsarClient) Close() {
	if c.producer != nil {
		c.producer.Close()
	}
	if c.consumer != nil {
		c.consumer.Close()
	}
	if c.client != nil {
		c.client.Close()
	}
}