package config

import (
	"fmt"
	"time"

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

	S3Region    string        `env:"S3_REGION" envDefault:"us-east-1"`
	S3AccessKey string        `env:"S3_ACCESS_KEY"`
	S3SecretKey string        `env:"S3_SECRET_KEY"`
	S3Bucket    string        `env:"S3_BUCKET"`
	S3Endpoint  string        `env:"S3_ENDPOINT"`
	S3Timeout   time.Duration `env:"S3_TIMEOUT" envDefault:"10s"` // Timeout for S3 requests in seconds

	ASREndpoint string        `env:"ASR_ENDPOINT" envDefault:"http://localhost:8080/asr"`
	ASRTimeout  time.Duration `env:"ASR_TIMEOUT" envDefault:"10s"` // Timeout for ASR requests in seconds

	OllamaProtocol string        `env:"OLLAMA_PROTOCOL" envDefault:"http"`
	OllamaHost     string        `env:"OLLAMA_HOST" envDefault:"localhost"`
	OllamaModel    string        `env:"OLLAMA_MODEL" envDefault:"llama3.1:8b"` // Model to use for Ollama
	OllamaTimeout  time.Duration `env:"OLLAMA_TIMEOUT" envDefault:"15s"`       // Timeout for Ollama requests in seconds

	DragonflyAddress        string        `env:"DRAGONFLY_ADDRESS" envDefault:"localhost:6379"`
	DragonflyPassword       string        `env:"DRAGONFLY_PASSWORD"`
	DragonflyDB             int           `env:"DRAGONFLY_DB" envDefault:"0"`
	DragonflyRequestTimeout time.Duration `env:"DRAGONFLY_REQUEST_TIMEOUT" envDefault:"1s"` // Timeout for Dragonfly requests in seconds

	TacticalChannelActivationDuration time.Duration `env:"TACTICAL_CHANNEL_ACTIVATION_DURATION" envDefault:"30m"` // Duration for which tactical channels are activated

	SlackToken     string        `env:"SLACK_TOKEN"`
	SlackChannelID string        `env:"SLACK_CHANNEL_ID"`
	SlackTimeout   time.Duration `env:"SLACK_TIMEOUT" envDefault:"5s"` // Timeout for Slack API requests in seconds

	WorkerCount int `env:"WORKER_COUNT" envDefault:"5"`

	WorkerTimeout time.Duration `env:"WORKER_TIMEOUT" envDefault:"30s"` // Timeout for each worker in seconds
}

func NewConfig() (*Config, error) {
	var cfg Config

	err := env.Parse(&cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	return &cfg, nil
}
