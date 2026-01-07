# testcontainers-go-redis-cluster

A Go library that provides a simple API for creating Redis clusters using [testcontainers-go](https://github.com/testcontainers/testcontainers-go) and the [grokzen/redis-cluster](https://github.com/Grokzen/docker-redis-cluster) Docker image. Perfect for integration testing with Redis clusters.

## Features

- 🚀 Simple API for Redis clusters
- 🔧 Configurable number of master nodes
- 🧹 Automatic cleanup via `t.Cleanup()`
- ✅ Returns connection strings compatible with `redis/go-redis` cluster client
- 🧪 Includes comprehensive tests

## Installation

```bash
go get github.com/NSXBet/testcontainers-go-redis-cluster
```

## Usage

### Redis Cluster

```go
package main

import (
    "context"
    "testing"
    
    "github.com/NSXBet/testcontainers-go-redis-cluster"
    "github.com/redis/go-redis/v9"
)

func TestMyFeature(t *testing.T) {
    // Create a 5-master Redis cluster (10 nodes total: 5 masters + 5 slaves)
    connStr := tcredis.Redis(t, 5)
    
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

### `tcredis.Redis(t *testing.T, nodes int) string`

Creates a Redis cluster with the specified number of master nodes and returns a connection string.

- **Parameters:**
  - `t`: The testing.T instance (used for cleanup)
  - `nodes`: Number of master nodes in the cluster (must be >= 1). Each master will have 1 slave by default.
- **Returns:** Connection string in `redis://host:port` format compatible with `redis.ParseClusterURL()`

**Note:** The number of nodes specified is the number of masters. The total number of nodes will be `2 * nodes` (masters + slaves). For example, `Redis(t, 5)` creates 5 masters and 5 slaves (10 nodes total).

## Requirements

- Go 1.21 or later
- Docker (required by testcontainers-go)

## Automatic Local Image Building

The library automatically builds a local Docker image (`tcredis/redis-cluster:7.0.7`) on first use. This ensures:

- **Better ARM64 compatibility**: No emulation needed on Apple Silicon
- **Faster subsequent runs**: Image is cached after first build
- **More reliable**: Locally built images work better with testcontainers
- **Transparent**: No manual steps required - just use `tcredis.Redis(t, 5)`

**First run:** The library will automatically:
1. Check if `tcredis/redis-cluster:7.0.7` exists locally
2. If not, clone the grokzen/docker-redis-cluster repository (cached in `/tmp/docker-redis-cluster`)
3. Build the image for your native platform (arm64 on Apple Silicon, amd64 on Intel)
4. Use the locally built image

**Subsequent runs:** The image is already built, so tests start immediately.

The repository is cached in `/tmp/docker-redis-cluster` and will be updated automatically when needed.

## How It Works

The library uses the [`grokzen/redis-cluster:7.0.7`](https://github.com/Grokzen/docker-redis-cluster) Docker image which automatically sets up a Redis cluster with the specified number of master nodes. The cluster is configured with:

- 1 slave per master (default behavior)
- Cluster mode enabled
- Automatic slot assignment
- Initial port: 10000
- Port mapping: `10000` to `10000 + 2 * nodes - 1` (e.g., for 5 nodes: ports 10000-10009)

The connection string returned includes all master node addresses. The `redis/go-redis` client will automatically discover all other nodes in the cluster when connecting.

## Testing

Run the tests:

```bash
go test ./...
```

Make sure Docker is running before executing tests.

## License

MIT
