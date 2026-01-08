package tcredis_test

import (
	"context"
	"testing"

	tcredis "github.com/NSXBet/testcontainers-go-redis-cluster"
	"github.com/redis/go-redis/v9"
)

// BenchmarkRedisV2Cluster benchmarks the RedisV2 cluster creation
func BenchmarkRedisV2Cluster(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		connStr := tcredis.RedisV2(b, 3)
		opts, err := redis.ParseClusterURL(connStr)
		if err != nil {
			b.Fatalf("Failed to parse connection string: %v", err)
		}
		client := redis.NewClusterClient(opts)
		ctx := context.Background()
		client.Set(ctx, "bench-key", "bench-value", 0)
		client.Get(ctx, "bench-key")
		client.Close()
	}
}
