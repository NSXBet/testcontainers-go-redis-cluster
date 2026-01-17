package tcredis_test

import (
	"context"
	"sync"
	"testing"
	"time"

	tcredis "github.com/NSXBet/testcontainers-go-redis-cluster"
	"github.com/redis/go-redis/v9"
)

// TestParallelRedisV3 verifies that multiple RedisV3 instances can run in parallel
// without port conflicts
func TestParallelRedisV3(t *testing.T) {
	var wg sync.WaitGroup
	results := make(chan bool, 5)

	// Start 5 tests in parallel
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(iteration int) {
			defer wg.Done()

			// Each iteration creates its own cluster
			// The port allocator should give each one a different port
			connStr := tcredis.RedisV3(t)

			opts, err := redis.ParseClusterURL(connStr)
			if err != nil {
				t.Errorf("Iteration %d: Failed to parse connection string: %v", iteration, err)
				results <- false
				return
			}

			client := redis.NewClusterClient(opts)
			defer client.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			// Do some work to keep the test running (simulates real test)
			key := "parallel-test-key"
			err = client.Set(ctx, key, "value", 0).Err()
			if err != nil {
				t.Errorf("Iteration %d: Failed to set key: %v", iteration, err)
				results <- false
				return
			}

			// Small delay to ensure overlap
			time.Sleep(100 * time.Millisecond)

			val, err := client.Get(ctx, key).Result()
			if err != nil {
				t.Errorf("Iteration %d: Failed to get key: %v", iteration, err)
				results <- false
				return
			}

			if val != "value" {
				t.Errorf("Iteration %d: Expected 'value', got '%s'", iteration, val)
				results <- false
				return
			}

			results <- true
		}(i)
	}

	wg.Wait()
	close(results)

	// Check all succeeded
	successCount := 0
	for success := range results {
		if success {
			successCount++
		}
	}

	if successCount != 5 {
		t.Fatalf("Expected 5 successful parallel tests, got %d", successCount)
	}

	t.Logf("All 5 parallel RedisV3 instances ran successfully!")
}
