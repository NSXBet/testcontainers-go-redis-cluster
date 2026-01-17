# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

This is a Go library that provides Redis cluster setup for tests using testcontainers-go. It offers three implementations:
- **RedisV3** (⭐ recommended): Ultra-fast, reliable emulated cluster using Dragonfly (~1s startup, 100% reliable)
- **Redis** (V1): Reliable multi-node Redis cluster with replicas (~22s startup, 100% reliable)
- **RedisV2**: Fast multi-node Redis cluster (~8s startup, 30-55% reliable due to Redis gossip protocol limitations)

## Common Commands

### Running Tests

**Important**: Tests have varying reliability depending on which implementation they test:
- RedisV3 tests: 100% pass rate (always succeed)
- Redis V1 tests: ~100% pass rate (always succeed)
- RedisV2 tests: ~30-55% pass rate (flaky due to Redis gossip protocol in Docker)

### Test Commands
```bash
# Run all tests
go test ./...

# Run tests with verbose output
go test -v ./...

# Run specific test
go test -v -run TestRedisV2Cluster

# Run benchmarks
go test -bench=. -benchmem
```

### Building and Linting
```bash
# Build the module
go build ./...

# Format code
go fmt ./...

# Run go vet
go vet ./...

# Tidy dependencies
go mod tidy
```

## Architecture

### Three Implementations

**RedisV3 (redis_v3.go)** - Modern, reliable approach:
- Single Dragonfly container in emulated cluster mode
- Responds to all Redis Cluster protocol commands
- No inter-container communication (single node)
- Uses `--cluster_mode=emulated` flag
- Fixed port mapping (26379:26379)
- No cluster-announce propagation issues
- ~1 second startup, 100% reliable in testing (141/141 tests passed)

### Two Implementations

**Redis V1 and V2:**

The library provides two distinct approaches to creating Redis clusters:

**RedisV2 (redis_v2.go)** - Fast, modern approach:
- Starts N separate Redis containers (one per master node)
- Creates a Docker network for inter-container communication
- Uses `redis-cli --cluster create` to form the cluster
- Configures `cluster-announce-ip` and `cluster-announce-port` for external client access
- No replicas (masters only)
- Uses official `redis:7.0.7` image
- Port range: 7000+

**Redis (redis.go)** - Legacy approach with replicas:
- Uses single container with `grokzen/redis-cluster` image
- Automatically builds local Docker image on first use
- Includes replicas (1 slave per master)
- Port range: 10000+
- Better ARM64 compatibility through local builds

### Cluster Announce Configuration

Both implementations solve the NAT/port mapping challenge where Redis containers use internal ports but clients connect via mapped external ports. The solution involves:

1. **Cluster Creation**: Use internal container IPs and ports for initial cluster formation
2. **Announce Configuration**: Set `cluster-announce-ip` and `cluster-announce-port` on each node to external addresses
3. **Verification**: Poll `CLUSTER SLOTS` to verify external addresses have propagated (this is what clients use for routing)
4. **Readiness**: Test actual operations to ensure clients can successfully route commands

### Key Functions

**RedisV2(t testing.TB, nodes int) string** (redis_v2.go):
- Main entry point for fast cluster setup
- Returns connection string in `redis://host:port?addr=host:port2&addr=host:port3` format
- Requires minimum 3 nodes (Redis cluster requirement)
- Uses parallel goroutines for announce configuration

**Redis(t testing.TB, nodes int) string** (redis.go):
- Main entry point for legacy cluster setup
- Returns same connection string format
- Creates 2*nodes total nodes (masters + slaves)
- Automatically builds Docker image if needed

**waitForClusterReadyV2 / waitForClusterReady**:
- Polls cluster until hash slots are fully assigned
- Checks `cluster_state:ok` and `cluster_slots_assigned:16384`

**waitForClusterAnnounceReadyV2 / waitForClusterAnnounceReady**:
- Critical function that verifies external addresses have propagated
- Uses `CLUSTER SLOTS` as authoritative source (what clients actually use)
- Requires consecutive successful checks to ensure stability
- Tests actual SET/GET operations to verify end-to-end functionality

### Cleanup

Both implementations register cleanup callbacks via `t.Cleanup()` to ensure:
- Containers are terminated when tests complete
- Docker networks are removed (RedisV2 only)
- Resources are cleaned up even on test failures

**Important**: RedisV2 network cleanup uses a fallback approach because testcontainers-go's `Network.Remove()` is deprecated. The cleanup first attempts the testcontainers API, then falls back to Docker CLI (`docker network rm`) if needed. This ensures networks are always cleaned up properly.

### Port Mapping Strategy

**RedisV2**:
- Internal: 7000, 7001, 7002...
- External: Docker-mapped random ports
- Announce: External mapped ports

**Redis V1**:
- Masters: 10000, 10002, 10004... (even offsets)
- Slaves: 10001, 10003, 10005... (odd offsets)
- External: Docker-mapped random ports
- Announce: External mapped ports

## Parallel Test Execution

**All implementations support parallel test execution** via an internal port allocator (ports.go):

- **Port Allocation**: Sequentially allocates ports starting from 27000 (configurable)
- **No Port Reuse**: Ports are NOT reused to avoid Docker async cleanup conflicts
- **Thread-Safe**: Uses sync.Mutex for concurrent access
- **Configurable**: Use functional options (`WithStartingPort()`) or global `SetStartingPort()`

**Why no port reuse?** Docker's `container.Terminate()` is async. Even after it returns, Docker may spend several seconds cleaning up network bindings. Reusing ports causes "port is already allocated" errors.

**How it works:**
1. Test requests port via `allocatePort()` or `allocatePortRange(n)`
2. Port allocator returns next sequential port (increments counter)
3. Parallel tests get different ports (27000, 27001, 27002, etc.)
4. Ports are never reused (port exhaustion not a concern - would need 30,000+ tests)

**Example:**
```go
// Test 1: allocates 27000
// Test 2 (parallel): allocates 27001
// Test 3 (parallel): allocates 27002
// Test 4 (after 1-3 complete): allocates 27003
// No reuse, no conflicts!
```

## Testing Notes

- Tests accept both `*testing.T` (unit tests) and `*testing.B` (benchmarks) via `testing.TB` interface
- Connection strings are compatible with `redis.ParseClusterURL()` from go-redis/v9
- RedisV2 has no replicas, so read scaling tests should use Redis V1
- Minimum 3 nodes required (enforced with clear error message)
- Docker must be running before tests
- **Parallel testing is fully supported** - no port conflicts

## Dependencies

- testcontainers-go v0.37.0
- go-redis/v9 v9.5.1
- Docker (runtime requirement)
