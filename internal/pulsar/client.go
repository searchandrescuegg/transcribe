package pulsar

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/apache/pulsar-client-go/pulsar"
	"github.com/apache/pulsar-client-go/pulsar/log"
)

type PulsarClient struct {
	client   pulsar.Client
	consumer pulsar.Consumer
}

func NewPulsarClient(url string, inputTopic string, subscription string) (*PulsarClient, error) {
	client, err := pulsar.NewClient(pulsar.ClientOptions{
		URL:    url,
		Logger: log.NewLoggerWithSlog(slog.Default()),
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

	return &PulsarClient{
		client:   client,
		consumer: consumer,
	}, nil
}

func (c *PulsarClient) Receive(ctx context.Context) (pulsar.Message, error) {
	return c.consumer.Receive(ctx)
}

func (c *PulsarClient) Ack(msg pulsar.Message) error {
	return c.consumer.Ack(msg)
}

func (c *PulsarClient) Nack(msg pulsar.Message) {
	c.consumer.Nack(msg)
}

func (c *PulsarClient) Close() {
	if c.consumer != nil {
		c.consumer.Close()
	}
	if c.client != nil {
		c.client.Close()
	}
}
