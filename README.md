# testcontainers-go-redis-cluster

A Go library for creating Redis clusters in tests using [testcontainers-go](https://github.com/testcontainers/testcontainers-go). Provides two implementations: `Redis` (V1) and `RedisV2` (recommended, faster).

## Quick Start

```go
package main

import (
    "context"
    "testing"
    
    "github.com/NSXBet/testcontainers-go-redis-cluster"
    "github.com/redis/go-redis/v9"
)

func TestMyFeature(t *testing.T) {
    // Recommended: Use RedisV2 for faster cluster startup (~8s vs ~22s)
    connStr := tcredis.RedisV2(t, 3) // 3-node cluster
    
    // Parse connection string and create cluster client
    opts, err := redis.ParseClusterURL(connStr)
    if err != nil {
        t.Fatalf("Failed to parse connection string: %v", err)
    }
    
    client := redis.NewClusterClient(opts)
    defer client.Close()
    
    // Use the cluster client
    ctx := context.Background()
    err = client.Set(ctx, "key", "value", 0).Err()
    if err != nil {
        t.Fatalf("Failed to set key: %v", err)
    }
    
    val, err := client.Get(ctx, "key").Result()
    if err != nil {
        t.Fatalf("Failed to get key: %v", err)
    }
    
    t.Logf("Got value: %s", val)
}
```

## API

### `tcredis.RedisV2(t testing.TB, nodes int) string` (Recommended)

Creates a Redis cluster by starting individual Redis containers and manually forming the cluster. This approach is **significantly faster** (~8 seconds) than V1.

**Parameters:**
- `t`: The testing.TB instance (used for cleanup). Accepts both `*testing.T` (unit tests) and `*testing.B` (benchmarks).
- `nodes`: Number of master nodes in the cluster (must be >= 3)

**Returns:** Connection string in `redis://host:port` format compatible with `redis.ParseClusterURL()`

**How it works:**
1. Starts N separate Redis containers (one per master node)
2. Creates a Docker network for inter-container communication
3. Forms the cluster using `redis-cli --cluster create`
4. Configures `cluster-announce-ip` and `cluster-announce-port` for external client access
5. Verifies cluster readiness and returns connection string

**Performance:** ~8 seconds for a 3-node cluster (vs ~22 seconds for V1)

**Ports:** Uses ports starting from 7000 (7000, 7001, 7002, etc.)

### `tcredis.Redis(t testing.TB, nodes int) string`

Creates a Redis cluster using the `grokzen/redis-cluster` Docker image. This is the original implementation.

**Parameters:**
- `t`: The testing.TB instance (used for cleanup). Accepts both `*testing.T` (unit tests) and `*testing.B` (benchmarks).
- `nodes`: Number of master nodes in the cluster (must be >= 3). Each master will have 1 slave by default.

**Returns:** Connection string in `redis://host:port` format compatible with `redis.ParseClusterURL()`

**How it works:**
1. Automatically builds a local Docker image (`tcredis/redis-cluster:7.0.7`) on first use
2. Starts a single container with the grokzen/redis-cluster image
3. The image automatically sets up the cluster with masters and slaves
4. Configures `cluster-announce-ip` and `cluster-announce-port` for external client access
5. Verifies cluster readiness and returns connection string

**Performance:** ~22 seconds for a 3-node cluster

**Note:** The number of nodes specified is the number of masters. The total number of nodes will be `2 * nodes` (masters + slaves). For example, `Redis(t, 5)` creates 5 masters and 5 slaves (10 nodes total).

**Ports:** Uses ports starting from 10000 (10000 to 10000 + 2 * nodes - 1)

## Comparison: RedisV2 vs Redis

| Feature          | RedisV2 (Recommended)       | Redis (V1)                           |
| ---------------- | --------------------------- | ------------------------------------ |
| **Startup Time** | ~8 seconds                  | ~22 seconds                          |
| **Approach**     | Individual containers       | Single container with grokzen image  |
| **Replicas**     | No replicas (masters only)  | 1 replica per master                 |
| **Image**        | Official `redis:7.0.7`      | Custom `tcredis/redis-cluster:7.0.7` |
| **Port Range**   | 7000+                       | 10000+                               |
| **Use Case**     | Faster tests, simpler setup | When replicas are needed             |

**Recommendation:** Use `RedisV2` unless you specifically need replicas. It's faster and uses the official Redis image.

## Requirements

- Go 1.21 or later
- Docker (required by testcontainers-go)

## Connection String Format

Both functions return connection strings in the format:
```
redis://host:port?addr=host:port2&addr=host:port3
```

This format is compatible with `redis.ParseClusterURL()` from the `github.com/redis/go-redis/v9` package.

## Automatic Image Building (Redis V1 only)

The `Redis` function automatically builds a local Docker image (`tcredis/redis-cluster:7.0.7`) on first use. This ensures:

- **Better ARM64 compatibility**: No emulation needed on Apple Silicon
- **Faster subsequent runs**: Image is cached after first build
- **More reliable**: Locally built images work better with testcontainers

**First run:** The library will automatically:
1. Check if `tcredis/redis-cluster:7.0.7` exists locally
2. If not, clone the grokzen/docker-redis-cluster repository (cached in `/tmp/docker-redis-cluster`)
3. Build the image for your native platform (arm64 on Apple Silicon, amd64 on Intel)
4. Use the locally built image

**Subsequent runs:** The image is already built, so tests start immediately.

The repository is cached in `/tmp/docker-redis-cluster` and will be updated automatically when needed.

**Note:** `RedisV2` uses the official `redis:7.0.7` image which is pulled automatically if not present.

## Cluster Requirements

- **Minimum nodes:** Redis clusters require at least 3 master nodes to form a proper cluster with hash slot assignment. Both `Redis` and `RedisV2` enforce this requirement and will fail with a clear error message if fewer than 3 nodes are requested.

## Testing

Run the tests:

```bash
go test ./...
```

Make sure Docker is running before executing tests.

## Examples

### Using RedisV2 (Recommended)

```go
func TestRedisCluster(t *testing.T) {
    connStr := tcredis.RedisV2(t, 3) // 3-node cluster
    
    opts, _ := redis.ParseClusterURL(connStr)
    client := redis.NewClusterClient(opts)
    defer client.Close()
    
    ctx := context.Background()
    client.Set(ctx, "key", "value", 0)
    val, _ := client.Get(ctx, "key").Result()
    t.Logf("Value: %s", val)
}
```

### Using Redis (V1)

```go
func TestRedisCluster(t *testing.T) {
    connStr := tcredis.Redis(t, 3) // 3 masters + 3 slaves = 6 nodes total
    
    opts, _ := redis.ParseClusterURL(connStr)
    client := redis.NewClusterClient(opts)
    defer client.Close()
    
    ctx := context.Background()
    client.Set(ctx, "key", "value", 0)
    val, _ := client.Get(ctx, "key").Result()
    t.Logf("Value: %s", val)
}
```

## License

MIT
