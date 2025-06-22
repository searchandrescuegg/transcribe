package config

import (
	"fmt"

	"github.com/caarlos0/env/v11"
)

type Config struct {
	LogLevel string `env:"LOG_LEVEL" envDefault:"error"`

	MetricsEnabled bool `env:"METRICS_ENABLED" envDefault:"true"`
	MetricsPort    int  `env:"METRICS_PORT" envDefault:"8081"`

	Local bool `env:"LOCAL" envDefault:"false"`

	TracingEnabled    bool    `env:"TRACING_ENABLED" envDefault:"false"`
	TracingSampleRate float64 `env:"TRACING_SAMPLERATE" envDefault:"0.01"`
	TracingService    string  `env:"TRACING_SERVICE" envDefault:"katalog-agent"`
	TracingVersion    string  `env:"TRACING_VERSION"`

	PulsarURL          string `env:"PULSAR_URL" envDefault:"pulsar://localhost:6650"`
	PulsarInputTopic   string `env:"PULSAR_INPUT_TOPIC" envDefault:"s3-events"`
	PulsarSubscription string `env:"PULSAR_SUBSCRIPTION" envDefault:"transcribe-consumer"`

	S3Region    string `env:"S3_REGION" envDefault:"us-east-1"`
	S3AccessKey string `env:"S3_ACCESS_KEY"`
	S3SecretKey string `env:"S3_SECRET_KEY"`
	S3Bucket    string `env:"S3_BUCKET"`
	S3Endpoint  string `env:"S3_ENDPOINT"`

	ASREndpoint string `env:"ASR_ENDPOINT" envDefault:"http://localhost:8080/asr"`

	OllamaProtocol string `env:"OLLAMA_PROTOCOL" envDefault:"http"`
	OllamaHost     string `env:"OLLAMA_HOST" envDefault:"localhost"`

	DragonflyAddress  string `env:"DRAGONFLY_ADDRESS" envDefault:"localhost:6379"`
	DragonflyPassword string `env:"DRAGONFLY_PASSWORD"`
	DragonflyDB       int    `env:"DRAGONFLY_DB" envDefault:"0"`

	SlackToken     string `env:"SLACK_TOKEN"`
	SlackChannelID string `env:"SLACK_CHANNEL_ID"`

	WorkerCount int `env:"WORKER_COUNT" envDefault:"5"`
}

func NewConfig() (*Config, error) {
	var cfg Config

	err := env.Parse(&cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	return &cfg, nil
}
