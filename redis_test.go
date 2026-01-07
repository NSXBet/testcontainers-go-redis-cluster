package tcredis_test

import (
	"context"
	"testing"
	"time"

	tcredis "github.com/NSXBet/testcontainers-go-redis-cluster"
	"github.com/redis/go-redis/v9"
)

func TestRedisCluster(t *testing.T) {
	connStr := tcredis.Redis(t, 3) // 3-node Redis cluster
	t.Logf("Connection string: %s", connStr)

	// Parse the connection string
	// redis://host:port?addr=host:port2&addr=host:port3 format
	opts, err := redis.ParseClusterURL(connStr)
	if err != nil {
		t.Fatalf("Failed to parse connection string: %v", err)
	}

	t.Logf("Cluster options - Addrs: %v", opts.Addrs)

	// First, verify we can connect to at least one node with a regular client
	// This ensures the container is actually accessible
	if len(opts.Addrs) > 0 {
		testClient := redis.NewClient(&redis.Options{
			Addr: opts.Addrs[0],
		})
		defer testClient.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := testClient.Ping(ctx).Err(); err != nil {
			cancel()
			t.Fatalf("Failed to ping first node %s: %v", opts.Addrs[0], err)
		}
		cancel()
		t.Logf("Successfully connected to first node: %s", opts.Addrs[0])
	}

	// Create cluster client with longer timeouts for initial connection
	opts.DialTimeout = 5 * time.Second
	opts.ReadTimeout = 5 * time.Second
	opts.WriteTimeout = 5 * time.Second
	client := redis.NewClusterClient(opts)
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// The cluster is already verified as ready in Redis() function
	// The cluster client may need a moment to initialize, but we can proceed with operations
	// Note: Due to Redis cluster NAT mapping, the client may have issues with topology discovery
	// but operations should still work since we provide all master addresses

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

	// Verify cluster is working by checking nodes

	// Test that we can connect to multiple nodes
	nodes, err := client.ClusterNodes(ctx).Result()
	if err != nil {
		t.Fatalf("Failed to get cluster nodes: %v", err)
	}
	t.Logf("Cluster has %d nodes", len(nodes))
}

func TestRedisClusterWithDifferentNodeCounts(t *testing.T) {
	// Note: Redis clusters require at least 3 master nodes to form a proper cluster
	// Single-node "clusters" will have cluster_state:fail and won't assign hash slots
	testCases := []struct {
		name  string
		nodes int
	}{
		{"3 nodes", 3},
		{"5 nodes", 5},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			connStr := tcredis.Redis(t, tc.nodes)

			opts, err := redis.ParseClusterURL(connStr)
			if err != nil {
				t.Fatalf("Failed to parse connection string: %v", err)
			}

			client := redis.NewClusterClient(opts)
			defer client.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			// Cluster is already ready (verified in Redis function), so we can use it immediately
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
