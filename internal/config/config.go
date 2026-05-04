package config

import (
	"fmt"
	"time"

	"github.com/caarlos0/env/v11"
)

type Config struct {
	// FIX (review item #15/16): default raised from "error" to "info" to match the README
	// and docker-compose.yml, which both already documented the effective default as info.
	LogLevel string `env:"LOG_LEVEL" envDefault:"info"`

	MetricsEnabled bool `env:"METRICS_ENABLED" envDefault:"true"`
	MetricsPort    int  `env:"METRICS_PORT" envDefault:"8081"`

	Local bool `env:"LOCAL" envDefault:"false"`

	// DisplayTimezone controls the timezone used when formatting timestamps that go to
	// Slack messages and logs (rescue alerts, thread replies, channel-closed notices).
	// Defaults to America/Los_Angeles which automatically swaps PST/PDT with daylight
	// saving — matches NORCOM's operating region. Set to UTC or any other IANA zone if
	// you operate elsewhere; an empty value falls back to the container's TZ.
	DisplayTimezone string `env:"DISPLAY_TIMEZONE" envDefault:"America/Los_Angeles"`

	TracingEnabled    bool    `env:"TRACING_ENABLED" envDefault:"false"`
	TracingSampleRate float64 `env:"TRACING_SAMPLERATE" envDefault:"0.01"`
	// FIX (review item #23): default was "katalog-agent" (copy-paste from another service),
	// which silently mislabeled traces in Tempo when TRACING_SERVICE was not set. Match the
	// binary name and the docker-compose override.
	TracingService string `env:"TRACING_SERVICE" envDefault:"transcribe"`
	// FIX (review item #23): default the version to "unset" instead of an empty string so
	// traces emitted without TRACING_VERSION are visibly tagged rather than carrying an
	// empty service.version attribute.
	TracingVersion string `env:"TRACING_VERSION" envDefault:"unset"`

	PulsarURL          string `env:"PULSAR_URL" envDefault:"pulsar://localhost:6650"`
	PulsarInputTopic   string `env:"PULSAR_INPUT_TOPIC" envDefault:"s3-events"`
	PulsarSubscription string `env:"PULSAR_SUBSCRIPTION" envDefault:"transcribe-consumer"`

	// FIX (review item #12): DLQ policy. Once the worker starts Nack'ing on processing failure
	// (see transcribe.go), unbounded retries can pin a partition on a poison message. After
	// PulsarMaxDeliveries failed attempts the message is moved to PulsarDLQTopic. Set
	// PulsarDLQTopic to an empty string to disable the DLQ and fall back to default (infinite
	// retry) — useful in environments where the DLQ topic hasn't been provisioned yet.
	PulsarDLQTopic            string        `env:"PULSAR_DLQ_TOPIC" envDefault:"public/transcribe/file-queue-dlq"`
	PulsarMaxDeliveries       uint32        `env:"PULSAR_MAX_DELIVERIES" envDefault:"5"`
	PulsarNackRedeliveryDelay time.Duration `env:"PULSAR_NACK_REDELIVERY_DELAY" envDefault:"30s"`

	S3Region    string        `env:"S3_REGION" envDefault:"us-east-1"`
	S3AccessKey string        `env:"S3_ACCESS_KEY"`
	S3SecretKey string        `env:"S3_SECRET_KEY"`
	S3Bucket    string        `env:"S3_BUCKET"`
	S3Endpoint  string        `env:"S3_ENDPOINT"`
	S3Timeout   time.Duration `env:"S3_TIMEOUT" envDefault:"10s"` // Timeout for S3 requests in seconds

	ASREndpoint string        `env:"ASR_ENDPOINT" envDefault:"http://localhost:8080/asr"`
	ASRTimeout  time.Duration `env:"ASR_TIMEOUT" envDefault:"10s"` // Timeout for ASR requests in seconds

	// OpenAI Configuration
	OpenAIAPIKey  string        `env:"OPENAI_API_KEY"`                                         // OpenAI API Key
	OpenAIBaseURL string        `env:"OPENAI_BASE_URL" envDefault:"https://api.openai.com/v1"` // OpenAI API Base URL
	OpenAIModel   string        `env:"OPENAI_MODEL" envDefault:"gpt-4"`                        // OpenAI Model to use
	OpenAITimeout time.Duration `env:"OPENAI_TIMEOUT" envDefault:"30s"`                        // Timeout for OpenAI requests in seconds

	// When false (default), the request includes
	// chat_template_kwargs: {"enable_thinking": false} which Qwen3-family chat templates
	// honor by skipping <think>...</think> emission entirely. Models without thinking
	// support (Gemma 4, etc.) silently ignore the unknown kwarg, so this is safe to
	// leave on universally. Flip to true if you ever serve a thinking model AND want
	// the chain-of-thought back (e.g. for higher-quality classification on harder cases).
	OpenAIEnableThinking bool `env:"OPENAI_ENABLE_THINKING" envDefault:"false"`

	// Confidential call-types list, decrypted at runtime.
	//
	// CallTypesPath is the path to the AES-256-GCM-encrypted file containing one call type
	// per line (post-decryption). When set, the list is loaded at startup and:
	//   - injected into the OpenAI system prompt so the model is told the canonical set, and
	//   - applied as an `enum` constraint on the response schema's call_type field, so the
	//     model can only return values from the list (plus "Unknown").
	// Leave CallTypesPath empty to disable the feature; the service then falls back to the
	// in-prompt example call types.
	//
	// CallTypesKey is the hex-encoded 32-byte AES key. Required iff CallTypesPath is set.
	// Generate one with: `go run ./cmd/encrypt-calltypes generate-key`.
	CallTypesPath string `env:"CALL_TYPES_PATH"`
	CallTypesKey  string `env:"CALL_TYPES_KEY"`

	DragonflyAddress        string        `env:"DRAGONFLY_ADDRESS" envDefault:"localhost:6379"`
	DragonflyPassword       string        `env:"DRAGONFLY_PASSWORD"`
	DragonflyDB             int           `env:"DRAGONFLY_DB" envDefault:"0"`
	DragonflyRequestTimeout time.Duration `env:"DRAGONFLY_REQUEST_TIMEOUT" envDefault:"1s"` // Timeout for Dragonfly requests in seconds

	TacticalChannelActivationDuration time.Duration `env:"TACTICAL_CHANNEL_ACTIVATION_DURATION" envDefault:"30m"` // Duration for which tactical channels are activated

	// FIX (review item #10 / option B): interval at which the durable TAC-expiry sweeper polls
	// the active_tacs ZSET for due "channel closed" notifications.
	TACSweeperInterval time.Duration `env:"TAC_SWEEPER_INTERVAL" envDefault:"5s"`

	// FIX (review item #11): TTL for the per-S3-object dedup key used to suppress duplicate
	// processing on Pulsar redelivery. Should comfortably exceed the worst-case end-to-end latency.
	DedupTTL time.Duration `env:"DEDUP_TTL" envDefault:"1h"`

	SlackToken                         string        `env:"SLACK_TOKEN"`
	SlackChannelID                     string        `env:"SLACK_CHANNEL_ID"`
	SlackTimeout                       time.Duration `env:"SLACK_TIMEOUT" envDefault:"5s"`                             // Timeout for Slack API requests in seconds
	SlackChannelClosedBroadcastEnabled bool          `env:"SLACK_CHANNEL_CLOSED_BROADCAST_ENABLED" envDefault:"false"` // Whether to broadcast channel closed messages

	// Socket Mode + interactivity (leave SlackAppToken empty to disable).
	//
	// SlackAppToken is the app-level token (xapp-...) generated in your Slack app's "Basic
	// Information" → "App-Level Tokens" with the connections:write scope. It opens an
	// outbound WebSocket so leadership can act on alerts without exposing a public HTTP
	// endpoint.
	//
	// SlackAllowedUserIDs is the comma-separated list of Slack user IDs (UXXXXXX) permitted
	// to press the Cancel / Extend buttons. Anyone else gets an ephemeral "not authorized"
	// reply. Empty list means nobody is authorized — useful as a safety default until you
	// explicitly grant access.
	SlackAppToken       string   `env:"SLACK_APP_TOKEN"`
	SlackAllowedUserIDs []string `env:"SLACK_ALLOWED_USER_IDS" envSeparator:","`

	// FeedbackFormURL is the Google Form viewform URL — when set, the closed rescue alert
	// gets a "Submit Feedback" button that opens the form with relevant fields prefilled.
	// Empty disables the feature; the closed alert renders without a feedback button.
	//
	// FeedbackFormFields is a JSON map of logical field names → Google Form entry IDs.
	// Recognized logical names: tac_channel, closed_at, dispatch_transcript, headline,
	// situation_summary. Operators inspect their form (browser dev tools on a prefilled
	// link) to find each question's entry.NNN id and supply the mapping.
	//
	// Example:
	//   FEEDBACK_FORM_URL=https://docs.google.com/forms/d/e/1FAIpQLSc.../viewform
	//   FEEDBACK_FORM_FIELDS={"tac_channel":"entry.111","closed_at":"entry.222","headline":"entry.333"}
	FeedbackFormURL    string `env:"FEEDBACK_FORM_URL"`
	FeedbackFormFields string `env:"FEEDBACK_FORM_FIELDS"`

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
