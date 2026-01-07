package tcredis_test

import (
	"context"
	"testing"
	"time"

	tcredis "github.com/NSXBet/testcontainers-go-redis-cluster"
	"github.com/redis/go-redis/v9"
)

func TestRedisV2Cluster(t *testing.T) {
	connStr := tcredis.RedisV2(t, 3) // 3-node Redis cluster
	t.Logf("Connection string: %s", connStr)

	// Parse the connection string
	opts, err := redis.ParseClusterURL(connStr)
	if err != nil {
		t.Fatalf("Failed to parse connection string: %v", err)
	}

	t.Logf("Cluster options - Addrs: %v", opts.Addrs)

	// Create cluster client
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

	// Test cluster info
	info, err := client.ClusterInfo(ctx).Result()
	if err != nil {
		t.Fatalf("Failed to get cluster info: %v", err)
	}
	t.Logf("Cluster info: %s", info)

	// Test cluster nodes
	nodes, err := client.ClusterNodes(ctx).Result()
	if err != nil {
		t.Fatalf("Failed to get cluster nodes: %v", err)
	}
	t.Logf("Cluster has %d nodes", len(nodes))
}

func TestRedisV2ClusterWithDifferentNodeCounts(t *testing.T) {
	// Note: Redis clusters require at least 3 master nodes to form a proper cluster
	testCases := []struct {
		name  string
		nodes int
	}{
		{"3 nodes", 3},
		{"5 nodes", 5},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			connStr := tcredis.RedisV2(t, tc.nodes)

			opts, err := redis.ParseClusterURL(connStr)
			if err != nil {
				t.Fatalf("Failed to parse connection string: %v", err)
			}

			client := redis.NewClusterClient(opts)
			defer client.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			// Test write and read
			key := "test-key-" + tc.name
			err = client.Set(ctx, key, "value", 0).Err()
			if err != nil {
				t.Fatalf("Failed to set key: %v", err)
			}

			val, err := client.Get(ctx, key).Result()
			if err != nil {
				t.Fatalf("Failed to get key: %v", err)
			}

			if val != "value" {
				t.Fatalf("Expected value 'value', got '%s'", val)
			}
		})
	}
}

