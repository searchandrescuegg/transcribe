package dragonfly

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

type DragonflyClient struct {
	client *redis.Client
}

func NewClient(ctx context.Context, opts *redis.Options) (*DragonflyClient, error) {
	redisClient := redis.NewClient(opts)
	_, err := redisClient.Ping(ctx).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to ping redis: %w", err)
	}

	return &DragonflyClient{redisClient}, nil
}

func (d *DragonflyClient) Close() error {
	return d.client.Close()
}

const DefaultExpiration = 30 * time.Minute

func (d *DragonflyClient) SAddEx(ctx context.Context, key string, ttl time.Duration, members ...interface{}) error {
	args := append([]interface{}{"SADDEX", key, ttl.Seconds()}, members...)
	return d.client.Do(ctx, args...).Err()
}

func (d *DragonflyClient) SMisMember(ctx context.Context, key string, members ...interface{}) ([]bool, error) {
	return d.client.SMIsMember(ctx, key, members).Result()
}

func (d *DragonflyClient) Set(ctx context.Context, key string, ttl time.Duration, value interface{}) error {
	return d.client.Set(ctx, key, value, ttl).Err()
}

func (d *DragonflyClient) Get(ctx context.Context, key string) (string, error) {
	value, err := d.client.Get(ctx, key).Result()
	if err == redis.Nil {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("failed to get value for key %s: %w", key, err)
	}
	return value, nil
}
