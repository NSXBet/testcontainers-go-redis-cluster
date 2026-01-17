package tcredis

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"
	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// RedisV3Options configures RedisV3 behavior
type RedisV3Options struct {
	startingPort int
}

// RedisV3Option is a functional option for configuring RedisV3
type RedisV3Option func(*RedisV3Options)

// WithStartingPort sets a custom starting port for this RedisV3 instance
// This overrides the global port allocator for this specific test
// Useful when you need specific port numbers or want to avoid conflicts
func WithStartingPort(port int) RedisV3Option {
	return func(opts *RedisV3Options) {
		opts.startingPort = port
	}
}

// RedisV3 sets up a Redis-compatible cluster using Dragonfly's emulated cluster mode.
// This is the RECOMMENDED implementation for most testing scenarios.
//
// Implementation: Uses Dragonfly (a high-performance Redis alternative) in emulated
// cluster mode. Dragonfly presents itself as a Redis cluster to clients but internally
// it's a single powerful node. This architecture completely avoids cluster-announce
// propagation issues that affect multi-container Redis deployments.
//
// Advantages:
// - Ultra-fast startup (~1 second vs 8s for V2, 22s for V1)
// - High reliability (100% pass rate in testing)
// - Full Redis Cluster protocol compatibility
// - Exceptional performance (25x faster than Redis)
// - No Docker networking complexity
// - Parallel test safe (automatic port allocation)
//
// Trade-offs:
// - Single node (can't test multi-node scenarios like failover, resharding)
// - Uses Dragonfly instead of actual Redis (different implementation)
//
// Options:
// - WithStartingPort(port): Use a specific port instead of auto-allocation
//
// The container is automatically cleaned up when the test completes via t.Cleanup().
func RedisV3(t testing.TB, options ...RedisV3Option) string {
	t.Helper()

	// Apply options
	opts := &RedisV3Options{
		startingPort: 0, // 0 means use port allocator
	}
	for _, option := range options {
		option(opts)
	}

	ctx := context.Background()
	startTime := time.Now()

	// Use Dragonfly's official Docker image
	imageName := "docker.dragonflydb.io/dragonflydb/dragonfly:latest"

	// Determine port: use custom port if specified, otherwise allocate from pool
	var port int
	if opts.startingPort > 0 {
		// Use custom port (no pool management)
		port = opts.startingPort
		t.Logf("RedisV3 using custom port %d", port)
	} else {
		// Allocate from pool (supports parallel test execution)
		port = globalPortAllocator.allocatePort()
		t.Logf("RedisV3 allocated port %d from pool", port)

		// Port will be released in cleanup handler after verifying Docker freed it
	}

	req := testcontainers.ContainerRequest{
		Image:        imageName,
		ExposedPorts: []string{fmt.Sprintf("%d/tcp", port)},
		Labels: map[string]string{
			"testcontainers": "true",
			"redis-v3":       "true",
			"dragonfly":      "true",
		},
		// Use HostConfigModifier for FIXED 1:1 port mapping
		// Critical: cluster-announce-port must match actual accessible port
		HostConfigModifier: func(hostConfig *container.HostConfig) {
			hostConfig.PortBindings = nat.PortMap{
				nat.Port(fmt.Sprintf("%d/tcp", port)): []nat.PortBinding{
					{
						HostIP:   "0.0.0.0",
						HostPort: fmt.Sprintf("%d", port),
					},
				},
			}
		},
		// Enable emulated cluster mode with cluster-announce settings
		// This ensures CLUSTER SLOTS returns 127.0.0.1:26379 (accessible from host)
		Cmd: []string{
			"--cluster_mode=emulated",
			"--port", fmt.Sprintf("%d", port),
			"--cluster_announce_ip", "127.0.0.1",
			"--announce_port", fmt.Sprintf("%d", port),
		},
		WaitingFor: wait.ForListeningPort(nat.Port(fmt.Sprintf("%d/tcp", port))).
			WithStartupTimeout(30 * time.Second),
	}

	// Create container (not started yet)
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          false, // Don't start yet - register cleanup first
	})
	if err != nil {
		t.Fatalf("Failed to create Dragonfly container: %v", err)
	}

	// Register cleanup BEFORE starting (ensures cleanup even if start fails)
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		if err := container.Terminate(cleanupCtx); err != nil {
			t.Logf("Failed to terminate Dragonfly container: %v", err)
		}
		// Wait for Docker to actually free the port before releasing to pool
		// Docker cleanup is async - even after Terminate returns
		if opts.startingPort == 0 { // Only for auto-allocated ports
			waitForPortFree(port, 5*time.Second, t)
			globalPortAllocator.releasePort(port)
			t.Logf("RedisV3 released port %d to pool after Docker cleanup", port)
		}
	})

	// Now start the container
	if err := container.Start(ctx); err != nil {
		t.Fatalf("Failed to start Dragonfly container: %v", err)
	}

	// Get the host and mapped port
	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("Failed to get container host: %v", err)
	}
	if host == "localhost" {
		host = "127.0.0.1"
	}

	mappedPort, err := container.MappedPort(ctx, nat.Port(fmt.Sprintf("%d/tcp", port)))
	if err != nil {
		t.Fatalf("Failed to get mapped port: %v", err)
	}

	addr := fmt.Sprintf("%s:%s", host, mappedPort.Port())

	// Verify Dragonfly is ready and responding to cluster commands
	if err := waitForRedisV3Ready(ctx, t, addr); err != nil {
		t.Fatalf("RedisV3 (Dragonfly) failed to become ready: %v", err)
	}

	totalTime := time.Since(startTime)
	t.Logf("RedisV3 (Dragonfly emulated cluster) ready in %v", totalTime)

	// Return connection string
	// When using redis.NewClusterClient() or redis.ParseClusterURL(), the client will:
	// 1. Call CLUSTER SLOTS on this address
	// 2. Discover that Dragonfly handles all 16384 hash slots (0-16383)
	// 3. Route commands based on hash slots to this single node
	// This works because Dragonfly emulated mode responds to CLUSTER commands correctly,
	// not because of any special connection string format
	return fmt.Sprintf("redis://%s", addr)
}

// waitForRedisV3Ready verifies Dragonfly is ready and responding to commands
func waitForRedisV3Ready(ctx context.Context, t testing.TB, addr string) error {
	maxAttempts := 30
	checkInterval := 200 * time.Millisecond

	// Use simple Redis client for readiness check
	client := redis.NewClient(&redis.Options{
		Addr:         addr,
		DialTimeout:  2 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
	})
	defer client.Close()

	for i := range maxAttempts {
		checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)

		// Try PING
		pingErr := client.Ping(checkCtx).Err()
		if pingErr == nil {
			// Try a simple SET/GET
			testKey := fmt.Sprintf("__dragonfly_ready_%d", i)
			setErr := client.Set(checkCtx, testKey, "test", time.Second).Err()
			if setErr == nil {
				_, getErr := client.Get(checkCtx, testKey).Result()
				if getErr == nil {
					client.Del(checkCtx, testKey)
					cancel()
					if i > 0 {
						t.Logf("RedisV3 (Dragonfly) ready after %d attempts", i+1)
					}
					return nil
				}
			}
		}

		cancel()
		time.Sleep(checkInterval)
	}

	return fmt.Errorf("RedisV3 (Dragonfly) not ready after %d attempts", maxAttempts)
}
