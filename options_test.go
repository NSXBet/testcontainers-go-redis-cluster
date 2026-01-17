package tcredis_test

import (
	"context"
	"strings"
	"testing"
	"time"

	tcredis "github.com/NSXBet/testcontainers-go-redis-cluster"
	"github.com/redis/go-redis/v9"
)

// TestRedisV3WithCustomPort verifies WithStartingPort option works
func TestRedisV3WithCustomPort(t *testing.T) {
	// Use a custom port via functional option
	connStr := tcredis.RedisV3(t, tcredis.WithStartingPort(38000))

	// Verify the connection string uses the custom port
	if !strings.Contains(connStr, ":38000") {
		t.Fatalf("Expected connection string to contain port 38000, got: %s", connStr)
	}

	// Verify it actually works
	opts, err := redis.ParseClusterURL(connStr)
	if err != nil {
		t.Fatalf("Failed to parse connection string: %v", err)
	}

	client := redis.NewClusterClient(opts)
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = client.Set(ctx, "custom-port-test", "works", 0).Err()
	if err != nil {
		t.Fatalf("Failed to set key: %v", err)
	}

	val, err := client.Get(ctx, "custom-port-test").Result()
	if err != nil {
		t.Fatalf("Failed to get key: %v", err)
	}

	if val != "works" {
		t.Fatalf("Expected 'works', got '%s'", val)
	}

	t.Logf("✅ WithStartingPort(38000) works correctly")
}

// TestRedisV2WithCustomPort verifies WithStartingPortV2 option works
func TestRedisV2WithCustomPort(t *testing.T) {
	// Use a custom port range via functional option
	connStr := tcredis.RedisV2(t, 3, tcredis.WithStartingPortV2(39000))

	// Verify the connection string uses ports in the custom range
	if !strings.Contains(connStr, ":39000") {
		t.Fatalf("Expected connection string to contain port 39000, got: %s", connStr)
	}

	t.Logf("✅ WithStartingPortV2(39000) allocates correct port range")
}
