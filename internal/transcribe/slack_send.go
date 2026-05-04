package transcribe

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/slack-go/slack"
)

// FIX (review item #1): the old handleSlackRateLimit returned nil after waiting RetryAfter
// but never actually re-sent the message, so any 429 silently dropped the alert. This wrapper
// performs a single bounded retry against the same channel; non-rate-limit errors return as-is
// and ctx cancellation aborts the wait. Returns the thread_ts of the posted message on success.
func (tc *TranscribeClient) sendSlackWithRetry(ctx context.Context, talkgroup string, opts ...slack.MsgOption) (string, error) {
	ts, err := tc.sendSlackOnce(ctx, opts...)
	if err == nil {
		return ts, nil
	}

	var rate *slack.RateLimitedError
	if !errors.As(err, &rate) || !rate.Retryable() {
		return "", err
	}

	slog.Warn("slack rate limited, retrying once",
		slog.Duration("retry_after", rate.RetryAfter),
		slog.String("talkgroup", talkgroup))

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(rate.RetryAfter):
	}

	return tc.sendSlackOnce(ctx, opts...)
}

func (tc *TranscribeClient) sendSlackOnce(ctx context.Context, opts ...slack.MsgOption) (string, error) {
	sendCtx, cancel := context.WithTimeout(ctx, tc.config.SlackTimeout)
	defer cancel()
	_, ts, _, err := tc.slackClient.SendMessageContext(sendCtx, tc.config.SlackChannelID, opts...)
	return ts, err
}
