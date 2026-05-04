package pulsar

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/apache/pulsar-client-go/pulsar"
	"github.com/apache/pulsar-client-go/pulsar/log"
)

type PulsarClient struct {
	client   pulsar.Client
	consumer pulsar.Consumer
}

// FIX (review item #12): poison messages used to redeliver forever once Work() started
// nacking on failure. Options now configures Pulsar's built-in dead-letter policy so that
// after MaxDeliveries failed attempts the message is published to DLQTopic and acked,
// keeping the main subscription unblocked. NackRedeliveryDelay paces transient retries.
type Options struct {
	URL                 string
	InputTopic          string
	Subscription        string
	DLQTopic            string
	MaxDeliveries       uint32
	NackRedeliveryDelay time.Duration
}

func NewPulsarClient(opts Options) (*PulsarClient, error) {
	// FIX (review item #15/16): the Pulsar logger is built from slog.Default() at construction
	// time, so it inherits whatever level main.go configured from c.LogLevel. Callers must set
	// the slog default before invoking NewPulsarClient or Pulsar will log at the zero level.
	client, err := pulsar.NewClient(pulsar.ClientOptions{
		URL:    opts.URL,
		Logger: log.NewLoggerWithSlog(slog.Default()),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create pulsar client: %w", err)
	}

	consumerOpts := pulsar.ConsumerOptions{
		Topic:               opts.InputTopic,
		SubscriptionName:    opts.Subscription,
		Type:                pulsar.Shared,
		NackRedeliveryDelay: opts.NackRedeliveryDelay,
	}
	// Only attach a DLQ policy if the operator configured one; otherwise fall back to the
	// Pulsar default (infinite retry) so this change is opt-in for environments without
	// a DLQ topic provisioned yet.
	if opts.DLQTopic != "" && opts.MaxDeliveries > 0 {
		consumerOpts.DLQ = &pulsar.DLQPolicy{
			MaxDeliveries:   opts.MaxDeliveries,
			DeadLetterTopic: opts.DLQTopic,
		}
	}

	consumer, err := client.Subscribe(consumerOpts)
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
