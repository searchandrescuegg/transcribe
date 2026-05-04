package dragonfly

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

type DragonflyClient struct {
	client         *redis.Client
	defaultTimeout time.Duration
}

func NewClient(ctx context.Context, defaultTimeout time.Duration, opts *redis.Options) (*DragonflyClient, error) {
	redisClient := redis.NewClient(opts)
	_, err := redisClient.Ping(ctx).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to ping redis: %w", err)
	}

	return &DragonflyClient{client: redisClient, defaultTimeout: defaultTimeout}, nil
}

func (d *DragonflyClient) Close() error {
	return d.client.Close()
}

const DefaultExpiration = 30 * time.Minute

func (d *DragonflyClient) SAddEx(ctx context.Context, key string, ttl time.Duration, members ...interface{}) error {
	dflyCtx, cancel := context.WithTimeout(ctx, d.defaultTimeout)
	defer cancel()

	args := append([]interface{}{"SADDEX", key, ttl.Seconds()}, members...)
	return d.client.Do(dflyCtx, args...).Err()
}

func (d *DragonflyClient) SMisMember(ctx context.Context, key string, members ...interface{}) ([]bool, error) {
	dflyCtx, cancel := context.WithTimeout(ctx, d.defaultTimeout)
	defer cancel()

	return d.client.SMIsMember(dflyCtx, key, members).Result()
}

func (d *DragonflyClient) Set(ctx context.Context, key string, ttl time.Duration, value interface{}) error {
	dflyCtx, cancel := context.WithTimeout(ctx, d.defaultTimeout)
	defer cancel()

	return d.client.Set(dflyCtx, key, value, ttl).Err()
}

func (d *DragonflyClient) Get(ctx context.Context, key string) (string, error) {
	dflyCtx, cancel := context.WithTimeout(ctx, d.defaultTimeout)
	defer cancel()

	value, err := d.client.Get(dflyCtx, key).Result()
	if err == redis.Nil {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("failed to get value for key %s: %w", key, err)
	}
	return value, nil
}

// FIX (review item #11): SetNX backs the at-most-once dedup guard for Pulsar redelivery,
// keyed by the S3 object key so re-processed events don't trigger duplicate Slack posts.
func (d *DragonflyClient) SetNX(ctx context.Context, key string, ttl time.Duration, value interface{}) (bool, error) {
	dflyCtx, cancel := context.WithTimeout(ctx, d.defaultTimeout)
	defer cancel()

	return d.client.SetNX(dflyCtx, key, value, ttl).Result()
}

// FIX (review item #10 / option B): ZAdd / ZRangeByScore / ZRem back the durable TAC-expiry sweeper,
// replacing the in-process time.AfterFunc that lost scheduled "channel closed" messages on restart.
func (d *DragonflyClient) ZAdd(ctx context.Context, key string, score float64, member string) error {
	dflyCtx, cancel := context.WithTimeout(ctx, d.defaultTimeout)
	defer cancel()

	return d.client.ZAdd(dflyCtx, key, redis.Z{Score: score, Member: member}).Err()
}

func (d *DragonflyClient) ZRangeByScore(ctx context.Context, key string, min, max string) ([]string, error) {
	dflyCtx, cancel := context.WithTimeout(ctx, d.defaultTimeout)
	defer cancel()

	return d.client.ZRangeByScore(dflyCtx, key, &redis.ZRangeBy{Min: min, Max: max}).Result()
}

// ZRem returns the number of members actually removed; the sweeper uses this as a
// claim primitive so each due closure is processed by exactly one goroutine.
func (d *DragonflyClient) ZRem(ctx context.Context, key string, member string) (int64, error) {
	dflyCtx, cancel := context.WithTimeout(ctx, d.defaultTimeout)
	defer cancel()

	return d.client.ZRem(dflyCtx, key, member).Result()
}

// SRem and Del back the cancel-from-Slack flow: when leadership marks a TAC as a false
// alarm, the controller must remove the talkgroup from the allow-list and drop the
// per-talkgroup routing key in addition to ZRem'ing the pending closure.
func (d *DragonflyClient) SRem(ctx context.Context, key string, members ...interface{}) error {
	dflyCtx, cancel := context.WithTimeout(ctx, d.defaultTimeout)
	defer cancel()

	return d.client.SRem(dflyCtx, key, members...).Err()
}

func (d *DragonflyClient) Del(ctx context.Context, keys ...string) error {
	dflyCtx, cancel := context.WithTimeout(ctx, d.defaultTimeout)
	defer cancel()

	return d.client.Del(dflyCtx, keys...).Err()
}

// RPush + LRange + Expire back the rolling-summary feature: each TAC transmission
// appends a JSON-encoded transcript entry to tac_transcripts:<TGID>; the summary path
// reads them all back in order before re-summarizing.
func (d *DragonflyClient) RPush(ctx context.Context, key string, values ...interface{}) error {
	dflyCtx, cancel := context.WithTimeout(ctx, d.defaultTimeout)
	defer cancel()

	return d.client.RPush(dflyCtx, key, values...).Err()
}

func (d *DragonflyClient) LRange(ctx context.Context, key string, start, stop int64) ([]string, error) {
	dflyCtx, cancel := context.WithTimeout(ctx, d.defaultTimeout)
	defer cancel()

	return d.client.LRange(dflyCtx, key, start, stop).Result()
}

// Expire (re)stamps a TTL on an existing key. Used after RPush since the LIST is created
// by the first push and inherits no TTL on its own.
func (d *DragonflyClient) Expire(ctx context.Context, key string, ttl time.Duration) error {
	dflyCtx, cancel := context.WithTimeout(ctx, d.defaultTimeout)
	defer cancel()

	return d.client.Expire(dflyCtx, key, ttl).Err()
}
