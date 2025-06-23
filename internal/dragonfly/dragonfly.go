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
