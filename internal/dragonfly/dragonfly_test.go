package dragonfly

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestSetXX_OnlyWritesExistingKeys_KeepsTTL exercises SetXX against a real Dragonfly (not a
// redis-server stand-in) since applyDispatchCorrection's safety against resurrecting a
// deleted tac_meta:<TGID> key depends on this exact "only if it exists, keep the TTL"
// semantics — a bug here would silently reopen the invariant-#6 hole that SetXX exists to
// close. Constrained per CLAUDE.md gotcha #3 so it can co-exist with a running dev stack.
func TestSetXX_OnlyWritesExistingKeys_KeepsTTL(t *testing.T) {
	ctx := context.Background()

	req := testcontainers.ContainerRequest{
		Image:        "docker.dragonflydb.io/dragonflydb/dragonfly:latest",
		ExposedPorts: []string{"6379/tcp"},
		Cmd:          []string{"--proactor_threads=2", "--maxmemory=512mb"},
		WaitingFor:   wait.ForListeningPort("6379/tcp").WithStartupTimeout(60 * time.Second),
	}
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err, "start dragonfly container")
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	host, err := container.Host(ctx)
	require.NoError(t, err)
	port, err := container.MappedPort(ctx, "6379")
	require.NoError(t, err)

	client, err := NewClient(ctx, 5*time.Second, &redis.Options{Addr: fmt.Sprintf("%s:%s", host, port.Port())})
	require.NoError(t, err, "connect to dragonfly")
	t.Cleanup(func() { _ = client.Close() })

	// Missing key: SetXX must report "not set" and must not create it.
	set, err := client.SetXX(ctx, "missing-key", "v1")
	require.NoError(t, err)
	require.False(t, set, "SetXX must not report success for a key that doesn't exist")
	exists, err := client.client.Exists(ctx, "missing-key").Result()
	require.NoError(t, err)
	require.EqualValues(t, 0, exists, "SetXX must not create the key")

	// Existing key: SetXX overwrites the value and keeps the pre-existing TTL.
	require.NoError(t, client.Set(ctx, "existing-key", time.Hour, "v1"))
	set, err = client.SetXX(ctx, "existing-key", "v2")
	require.NoError(t, err)
	require.True(t, set, "SetXX must report success for an existing key")

	got, err := client.Get(ctx, "existing-key")
	require.NoError(t, err)
	require.Equal(t, "v2", got)

	ttl, err := client.client.TTL(ctx, "existing-key").Result()
	require.NoError(t, err)
	require.Greater(t, ttl, 55*time.Minute, "SetXX must keep the existing TTL, not reset it")
}
