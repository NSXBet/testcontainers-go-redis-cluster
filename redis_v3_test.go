package tcredis_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	tcredis "github.com/NSXBet/testcontainers-go-redis-cluster"
	"github.com/redis/go-redis/v9"
)

func TestRedisV3Cluster(t *testing.T) {
	connStr := tcredis.RedisV3(t)
	t.Logf("Connection string: %s", connStr)

	// Parse the connection string as a cluster URL
	opts, err := redis.ParseClusterURL(connStr)
	if err != nil {
		t.Fatalf("Failed to parse connection string: %v", err)
	}

	t.Logf("Cluster options - Addrs: %v", opts.Addrs)

	// Create cluster client
	// RedisV3 uses Dragonfly emulated mode which responds to cluster commands
	opts.DialTimeout = 5 * time.Second
	opts.ReadTimeout = 5 * time.Second
	opts.WriteTimeout = 5 * time.Second
	client := redis.NewClusterClient(opts)
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Test basic operations
	err = client.Set(ctx, "test-key", "test-value", 0).Err()
	if err != nil {
		t.Fatalf("Failed to set key: %v", err)
	}

	val, err := client.Get(ctx, "test-key").Result()
	if err != nil {
		t.Fatalf("Failed to get key: %v", err)
	}

	if val != "test-value" {
		t.Fatalf("Expected value 'test-value', got '%s'", val)
	}

	// Test cluster info (RedisV3/Dragonfly responds to cluster commands)
	info, err := client.ClusterInfo(ctx).Result()
	if err != nil {
		t.Fatalf("Failed to get cluster info: %v", err)
	}
	t.Logf("Cluster info: %s", info)

	// Test that we can write and read multiple keys (hash slot distribution)
	for i := 0; i < 10; i++ {
		key := fmt.Sprintf("test-key-%d", i)
		err := client.Set(ctx, key, fmt.Sprintf("value-%d", i), 0).Err()
		if err != nil {
			t.Fatalf("Failed to set key %s: %v", key, err)
		}

		val, err := client.Get(ctx, key).Result()
		if err != nil {
			t.Fatalf("Failed to get key %s: %v", key, err)
		}

		expected := fmt.Sprintf("value-%d", i)
		if val != expected {
			t.Fatalf("Expected value '%s', got '%s'", expected, val)
		}
	}

	t.Logf("Successfully tested emulated cluster mode with multiple keys")
}

func TestRedisV3ClusterReliability(t *testing.T) {
	// Run multiple iterations to verify high reliability
	iterations := 5
	for i := 0; i < iterations; i++ {
		t.Run(fmt.Sprintf("iteration-%d", i), func(t *testing.T) {
			connStr := tcredis.RedisV3(t)

			opts, err := redis.ParseClusterURL(connStr)
			if err != nil {
				t.Fatalf("Failed to parse connection string: %v", err)
			}

			client := redis.NewClusterClient(opts)
			defer client.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			// Quick test - should always work
			key := fmt.Sprintf("reliability-test-%d", i)
			err = client.Set(ctx, key, "reliable", 0).Err()
			if err != nil {
				t.Fatalf("Failed to set key: %v", err)
			}

			val, err := client.Get(ctx, key).Result()
			if err != nil {
				t.Fatalf("Failed to get key: %v", err)
			}

			if val != "reliable" {
				t.Fatalf("Expected 'reliable', got '%s'", val)
			}
		})
	}
}
