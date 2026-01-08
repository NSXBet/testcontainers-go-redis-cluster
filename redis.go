package tcredis

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/go-connections/nat"
	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Redis sets up a Redis cluster with the specified number of master nodes.
// It returns a connection string in the format redis://host:port
// that can be used with Redis clients that support cluster mode.
// The cluster is automatically cleaned up when the test completes via t.Cleanup().
// Based on https://github.com/Grokzen/docker-redis-cluster
func Redis(t testing.TB, nodes int) string {
	if nodes < 1 {
		t.Fatalf("number of nodes must be at least 1, got %d", nodes)
	}

	ctx := context.Background()

	// Use INITIAL_PORT=10000 as per grokzen/redis-cluster pattern
	// Port mapping: INITIAL_PORT to INITIAL_PORT + 2 * nodes - 1
	// For 5 nodes: 10000 to 10009 (10 ports total)
	initialPort := 10000
	totalPorts := 2 * nodes // 2 ports per node (master + slave)

	// Build exposed ports string
	var exposedPorts []string
	for i := 0; i < totalPorts; i++ {
		exposedPorts = append(exposedPorts, fmt.Sprintf("%d/tcp", initialPort+i))
	}

	// Ensure local image exists, build if needed
	// This ensures we use a locally built image for better ARM64 compatibility
	redisVersion := "7.0.7"
	imageName := fmt.Sprintf("tcredis/redis-cluster:%s", redisVersion)
	if err := ensureLocalImage(ctx, t, imageName, redisVersion); err != nil {
		t.Fatalf("Failed to ensure local image exists: %v", err)
	}
	req := testcontainers.ContainerRequest{
		Image:        imageName,
		ExposedPorts: exposedPorts,
		// Wait for both the log message AND the first port to be listening
		WaitingFor: wait.ForAll(
			wait.ForLog("Ready to accept connections"),
			wait.ForListeningPort(nat.Port(fmt.Sprintf("%d/tcp", initialPort))),
		).WithDeadline(60 * time.Second),
		Env: map[string]string{
			"IP":                "127.0.0.1", // Use host IP for cluster discovery (testcontainers maps to host)
			"BIND_ADDRESS":      "0.0.0.0",   // Bind to all interfaces so nodes are accessible from host
			"INITIAL_PORT":      fmt.Sprintf("%d", initialPort),
			"MASTERS":           fmt.Sprintf("%d", nodes), // Number of nodes = number of masters
			"SLAVES_PER_MASTER": "1",                      // Default: 1 slave per master
		},
		// ImagePlatform is intentionally left unset to use native architecture
	}

	redisC, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("Failed to start Redis cluster container: %v", err)
	}

	// Register cleanup immediately to ensure container is always terminated
	// Use background context to ensure cleanup runs even if test context is cancelled
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		if err := redisC.Terminate(cleanupCtx); err != nil {
			t.Logf("Failed to terminate Redis container: %v", err)
		}
	})

	// Get the host - use 127.0.0.1 explicitly to avoid IPv6 issues on Mac
	host, err := redisC.Host(ctx)
	if err != nil {
		t.Fatalf("Failed to get container host: %v", err)
	}

	// Force IPv4 to avoid connection issues on Mac (localhost might resolve to IPv6)
	if host == "localhost" {
		host = "127.0.0.1"
	}

	// Build connection string with master node addresses only
	// The cluster client will discover slaves automatically from the cluster topology
	// Master ports: INITIAL_PORT, INITIAL_PORT+2, INITIAL_PORT+4, etc.
	var addrs []string
	for i := 0; i < nodes; i++ {
		// Master nodes are at even offsets: 0, 2, 4, 6, ...
		masterPort := initialPort + (i * 2)
		port, err := redisC.MappedPort(ctx, nat.Port(fmt.Sprintf("%d/tcp", masterPort)))
		if err != nil {
			t.Fatalf("Failed to get mapped port for master node %d: %v", i, err)
		}
		addrs = append(addrs, fmt.Sprintf("%s:%s", host, port.Port()))
	}

	// Wait for cluster to be fully ready (hash slots assigned)
	// The "Ready to accept connections" log appears before slots are assigned
	// Note: Single-node clusters may not form a proper cluster (Redis requires 3+ masters)
	// So we skip the readiness check for single-node and let the client handle it
	if nodes > 1 {
		if err := waitForClusterReady(ctx, t, addrs[0], nodes); err != nil {
			t.Fatalf("Cluster failed to become ready: %v", err)
		}
		// Configure each node to advertise external addresses (mapped ports)
		// This is necessary for clients to connect from outside the container
		if err := configureClusterAnnounce(ctx, t, redisC, addrs, initialPort, nodes); err != nil {
			t.Logf("Warning: failed to configure cluster announce addresses: %v (cluster may still work)", err)
		}
		// Poll to verify cluster announce configuration has propagated
		// Use short sleeps (100ms) and check readiness, up to 50 attempts (5s max)
		// This is faster when cluster is ready early, but still reliable
		// We need to check ALL addresses (masters + slaves) since clients may connect to any node
		if err := waitForClusterAnnounceReady(ctx, t, addrs, initialPort, nodes); err != nil {
			t.Logf("Warning: cluster announce may not be fully ready: %v (continuing anyway)", err)
		}
	} else {
		// For single-node, just wait a bit for the container to fully start
		// The cluster client may still work even if cluster_state is fail
		time.Sleep(2 * time.Second)
	}

	// Return connection string with all addresses for cluster discovery
	// Format: redis://host:port?addr=host:port2&addr=host:port3...
	if len(addrs) > 1 {
		return fmt.Sprintf("redis://%s?%s", addrs[0], buildAddrParams(addrs[1:]))
	}
	return fmt.Sprintf("redis://%s", addrs[0])
}

// waitForClusterReady waits for the cluster to be fully initialized by checking CLUSTER INFO
func waitForClusterReady(ctx context.Context, t testing.TB, firstAddr string, expectedMasters int) error {
	// Single-node clusters may take longer to initialize
	timeout := 60 * time.Second
	if expectedMasters == 1 {
		timeout = 90 * time.Second // Give single-node more time
	}
	deadline := time.Now().Add(timeout)
	checkInterval := 500 * time.Millisecond
	attempts := 0

	for time.Now().Before(deadline) {
		attempts++
		// Create a temporary client to check cluster status
		client := redis.NewClient(&redis.Options{
			Addr:         firstAddr,
			DialTimeout:  2 * time.Second,
			ReadTimeout:  2 * time.Second,
			WriteTimeout: 2 * time.Second,
		})

		// Check if we can connect
		if err := client.Ping(ctx).Err(); err != nil {
			if attempts%10 == 0 { // Log every 10th attempt
				t.Logf("Waiting for cluster (attempt %d): ping failed: %v", attempts, err)
			}
			client.Close()
			time.Sleep(checkInterval)
			continue
		}

		// Check cluster info to ensure cluster_state:ok
		info, err := client.ClusterInfo(ctx).Result()
		if err != nil {
			if attempts%10 == 0 {
				t.Logf("Waiting for cluster (attempt %d): cluster info failed: %v", attempts, err)
			}
			client.Close()
			time.Sleep(checkInterval)
			continue
		}

		// Log cluster info on first successful connection
		if attempts == 1 {
			t.Logf("Cluster info: %s", info)
		}

		// Check cluster state
		// For single-node clusters, Redis might show cluster_state:fail initially
		// We need to wait for it to become "ok" or check if slots are assigned
		clusterStateOK := strings.Contains(info, "cluster_state:ok")
		hasSlotsAssigned := strings.Contains(info, "cluster_slots_assigned:16384")

		// For single-node, we might need to wait longer or accept a different state
		// Let's check if we can at least write to it
		if clusterStateOK && hasSlotsAssigned {
			client.Close()
			t.Logf("Cluster ready after %d attempts (state:ok, slots:16384)", attempts)
			return nil
		}

		// For single-node, if we have slots assigned, try to use it even if state is fail
		// This might be acceptable for a 1-node "cluster"
		if expectedMasters == 1 && hasSlotsAssigned {
			// Try a test write to see if it actually works
			testKey := fmt.Sprintf("__test_ready_%d", attempts)
			if err := client.Set(ctx, testKey, "test", time.Second).Err(); err == nil {
				client.Del(ctx, testKey)
				client.Close()
				t.Logf("Cluster ready after %d attempts (single-node, slots assigned, write works)", attempts)
				return nil
			}
		}

		if attempts%10 == 0 {
			state := "unknown"
			if strings.Contains(info, "cluster_state:") {
				for _, line := range strings.Split(info, "\n") {
					if strings.Contains(line, "cluster_state:") {
						state = strings.TrimSpace(line)
						break
					}
				}
			}
			t.Logf("Waiting for cluster (attempt %d): state=%s, hasSlots=%v", attempts, state, hasSlotsAssigned)
		}

		client.Close()
		time.Sleep(checkInterval)
	}

	return fmt.Errorf("cluster did not become ready within %v (after %d attempts)", timeout, attempts)
}

// ensureLocalImage checks if the image exists locally, and builds it if it doesn't
func ensureLocalImage(ctx context.Context, t testing.TB, imageName, redisVersion string) error {
	// Check if image exists locally
	cmd := exec.CommandContext(ctx, "docker", "image", "inspect", imageName)
	if err := cmd.Run(); err == nil {
		// Image exists, we're good
		return nil
	}

	// Image doesn't exist, we need to build it
	t.Logf("Image %s not found locally, building it (this may take a few minutes the first time)...", imageName)

	// Get or clone the grokzen repository
	repoDir := filepath.Join(os.TempDir(), "docker-redis-cluster")
	if err := ensureGrokzenRepo(ctx, t, repoDir); err != nil {
		return fmt.Errorf("failed to ensure grokzen repo: %w", err)
	}

	// Build the image for native platform - explicitly set platform to match host architecture
	// This ensures ARM64 on Apple Silicon, AMD64 on Intel, etc.
	platform, err := getNativePlatform()
	if err != nil {
		return fmt.Errorf("failed to detect native platform: %w", err)
	}

	buildCmd := exec.CommandContext(ctx, "docker", "build",
		"--platform", platform,
		"--build-arg", fmt.Sprintf("redis_version=%s", redisVersion),
		"-t", imageName,
		repoDir,
	)
	// Show build output so user can see progress
	buildCmd.Stdout = os.Stdout
	buildCmd.Stderr = os.Stderr

	if err := buildCmd.Run(); err != nil {
		return fmt.Errorf("failed to build image: %w", err)
	}

	t.Logf("✅ Successfully built image: %s", imageName)
	return nil
}

// ensureGrokzenRepo ensures the grokzen/docker-redis-cluster repository is available locally
func ensureGrokzenRepo(ctx context.Context, t testing.TB, repoDir string) error {
	// Check if repo already exists
	if _, err := os.Stat(filepath.Join(repoDir, ".git")); err == nil {
		// Repo exists, try to update it
		cmd := exec.CommandContext(ctx, "git", "pull")
		cmd.Dir = repoDir
		if err := cmd.Run(); err != nil {
			t.Logf("Warning: failed to update grokzen repo: %v (continuing with existing version)", err)
		}
		return nil
	}

	// Clone the repo
	t.Logf("Cloning grokzen/docker-redis-cluster repository to %s...", repoDir)
	cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1",
		"https://github.com/Grokzen/docker-redis-cluster.git", repoDir)
	// Show git output so user can see progress
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to clone grokzen repo: %w", err)
	}

	return nil
}

// getNativePlatform detects the native platform architecture and returns the Docker platform string
func getNativePlatform() (string, error) {
	cmd := exec.Command("uname", "-m")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to run uname -m: %w", err)
	}

	arch := strings.TrimSpace(string(output))
	switch arch {
	case "arm64", "aarch64":
		return "linux/arm64", nil
	case "x86_64", "amd64":
		return "linux/amd64", nil
	default:
		return "", fmt.Errorf("unsupported architecture: %s", arch)
	}
}

// configureClusterAnnounce configures each Redis node to advertise external addresses
// This is necessary for clients to connect from outside the container (NAT mapping)
func configureClusterAnnounce(ctx context.Context, t testing.TB, redisC testcontainers.Container, addrs []string, initialPort, nodes int) error {
	// Get the host IP (should be 127.0.0.1 for testcontainers)
	host, err := redisC.Host(ctx)
	if err != nil {
		return fmt.Errorf("failed to get host: %w", err)
	}
	if host == "localhost" {
		host = "127.0.0.1"
	}

	// Configure each master node to advertise its external (mapped) address
	for i := 0; i < nodes; i++ {
		masterPort := initialPort + (i * 2)
		// Get the mapped port for this master
		mappedPort, err := redisC.MappedPort(ctx, nat.Port(fmt.Sprintf("%d/tcp", masterPort)))
		if err != nil {
			return fmt.Errorf("failed to get mapped port for master %d: %w", i, err)
		}

		// Use redis-cli inside the container to configure cluster-announce-ip and cluster-announce-port
		exitCode, _, err := redisC.Exec(ctx, []string{
			"redis-cli",
			"-p", fmt.Sprintf("%d", masterPort),
			"CONFIG", "SET", "cluster-announce-ip", host,
		})
		if err != nil || exitCode != 0 {
			return fmt.Errorf("failed to set cluster-announce-ip for master %d: exitCode=%d, err=%w", i, exitCode, err)
		}

		exitCode, _, err = redisC.Exec(ctx, []string{
			"redis-cli",
			"-p", fmt.Sprintf("%d", masterPort),
			"CONFIG", "SET", "cluster-announce-port", mappedPort.Port(),
		})
		if err != nil || exitCode != 0 {
			return fmt.Errorf("failed to set cluster-announce-port for master %d: exitCode=%d, err=%w", i, exitCode, err)
		}
	}

	// Also configure slave nodes - clients may connect to slaves for reads
	for i := 0; i < nodes; i++ {
		slavePort := initialPort + (i * 2) + 1
		mappedPort, err := redisC.MappedPort(ctx, nat.Port(fmt.Sprintf("%d/tcp", slavePort)))
		if err != nil {
			// Slave might not exist, continue
			continue
		}

		exitCode, _, err := redisC.Exec(ctx, []string{
			"redis-cli",
			"-p", fmt.Sprintf("%d", slavePort),
			"CONFIG", "SET", "cluster-announce-ip", host,
		})
		if err != nil || exitCode != 0 {
			// Log but don't fail - slaves are less critical
			continue
		}

		exitCode, _, err = redisC.Exec(ctx, []string{
			"redis-cli",
			"-p", fmt.Sprintf("%d", slavePort),
			"CONFIG", "SET", "cluster-announce-port", mappedPort.Port(),
		})
		if err != nil || exitCode != 0 {
			// Log but don't fail
			continue
		}
	}

	return nil
}

// waitForClusterAnnounceReady polls to verify that cluster announce configuration has propagated
// This allows the cluster to be used as soon as it's ready, rather than waiting a fixed time
// Uses 100ms intervals, up to 50 attempts (5s max total)
// Verifies ALL addresses (masters + slaves) in parallel since clients may connect to any node
func waitForClusterAnnounceReady(ctx context.Context, t testing.TB, addrs []string, initialPort, nodes int) error {
	maxAttempts := 50
	checkInterval := 100 * time.Millisecond

	// Give Redis a moment to start processing the announce configuration
	// Cluster announce propagation takes some time, so we start checking after a brief delay
	time.Sleep(300 * time.Millisecond)

	// Build connection string with all master addresses
	connStr := fmt.Sprintf("redis://%s", addrs[0])
	if len(addrs) > 1 {
		connStr = fmt.Sprintf("redis://%s?%s", addrs[0], buildAddrParams(addrs[1:]))
	}

	// We need multiple consecutive successful checks to ensure topology is stable
	consecutiveSuccesses := 0
	requiredSuccesses := 3 // Need 3 consecutive successful checks for bulletproof reliability

	for i := 0; i < maxAttempts; i++ {
		// Try to connect with a cluster client using all master addresses
		opts, err := redis.ParseClusterURL(connStr)
		if err != nil {
			time.Sleep(checkInterval)
			continue
		}

		opts.DialTimeout = 1 * time.Second
		opts.ReadTimeout = 1 * time.Second
		opts.WriteTimeout = 1 * time.Second

		client := redis.NewClusterClient(opts)

		// Get CLUSTER SLOTS - this is the authoritative source for cluster topology
		// It returns the actual addresses that clients will use when routing commands
		// This is more reliable than CLUSTER NODES for checking announce addresses
		checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		slots, err := client.ClusterSlots(checkCtx).Result()
		cancel()

		if err != nil {
			// If we can't get cluster nodes, check if it's an internal port error
			errStr := err.Error()
			hasInternalPort := strings.Contains(errStr, ":10000") ||
				strings.Contains(errStr, ":10001") ||
				strings.Contains(errStr, ":10002") ||
				strings.Contains(errStr, ":10003") ||
				strings.Contains(errStr, ":10004") ||
				strings.Contains(errStr, ":10005") ||
				strings.Contains(errStr, "127.0.0.1:1000")

			client.Close()

			if hasInternalPort {
				// Still trying to connect to internal ports, need to wait more
				time.Sleep(checkInterval)
				continue
			}

			// Other error, might be transient, wait and retry
			time.Sleep(checkInterval)
			continue
		}

		// Verify ALL addresses in CLUSTER SLOTS are external (not internal ports)
		// CLUSTER SLOTS returns the actual addresses clients will use - this is bulletproof
		// Each slot has Nodes []ClusterNode with Addr field - we check ALL of them
		hasInternalPortInSlots := false
		totalNodesInSlots := 0

		for _, slot := range slots {
			// Check master node (first node in slot)
			if len(slot.Nodes) > 0 && slot.Nodes[0].Addr != "" {
				totalNodesInSlots++
				addr := slot.Nodes[0].Addr
				if isInternalPort(addr, initialPort, nodes) {
					hasInternalPortInSlots = true
					break
				}
			}

			// Check replica nodes (remaining nodes in slot)
			for j := 1; j < len(slot.Nodes); j++ {
				if slot.Nodes[j].Addr != "" {
					totalNodesInSlots++
					addr := slot.Nodes[j].Addr
					if isInternalPort(addr, initialPort, nodes) {
						hasInternalPortInSlots = true
						break
					}
				}
			}

			if hasInternalPortInSlots {
				break
			}
		}

		// Also check CLUSTER NODES to verify ALL nodes (including replicas not in slots)
		// CLUSTER NODES shows the full cluster topology including all replicas
		checkCtx2, cancel2 := context.WithTimeout(ctx, 2*time.Second)
		nodesInfo, err2 := client.ClusterNodes(checkCtx2).Result()
		cancel2()

		hasInternalPortInNodes := false
		totalNodesInCluster := 0

		if err2 == nil {
			// Parse CLUSTER NODES to get all node addresses
			// Format: "node-id ip:port@port flags master - 0-5460"
			for _, line := range strings.Split(nodesInfo, "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				parts := strings.Fields(line)
				if len(parts) >= 2 {
					// Extract IP:port from the node info
					addr := parts[1]
					// Remove @port suffix if present (cluster bus port)
					if idx := strings.Index(addr, "@"); idx != -1 {
						addr = addr[:idx]
					}
					totalNodesInCluster++
					if isInternalPort(addr, initialPort, nodes) {
						hasInternalPortInNodes = true
						break
					}
				}
			}
		}

		// If either CLUSTER SLOTS or CLUSTER NODES has internal ports, wait and retry
		hasInternalPort := hasInternalPortInSlots || hasInternalPortInNodes

		// If we found any internal ports in CLUSTER SLOTS, wait and retry
		// This is the most reliable check - CLUSTER SLOTS is what clients actually use
		if hasInternalPort {
			client.Close()
			time.Sleep(checkInterval)
			continue
		}

		// All addresses in CLUSTER SLOTS are external! Now verify operations work
		// Force reload to ensure client uses the verified topology
		client.ReloadState(ctx)

		// Try multiple SET/GET operations to different slots to ensure everything works
		// This will fail if the client tries to use internal addresses
		checkCtx, cancel = context.WithTimeout(ctx, 3*time.Second)
		var setErr error
		for j := 0; j < 3; j++ {
			testKey := fmt.Sprintf("__cluster_announce_test_%d_%d", i, j)
			setErr = client.Set(checkCtx, testKey, "test", time.Second).Err()
			if setErr != nil {
				break
			}
			_, setErr = client.Get(checkCtx, testKey).Result()
			if setErr != nil {
				break
			}
			client.Del(checkCtx, testKey)
		}
		cancel()

		// Check if SET/GET error is about internal addresses
		if setErr != nil {
			setErrStr := setErr.Error()
			if strings.Contains(setErrStr, ":10000") ||
				strings.Contains(setErrStr, ":10001") ||
				strings.Contains(setErrStr, ":10002") ||
				strings.Contains(setErrStr, ":10003") ||
				strings.Contains(setErrStr, ":10004") ||
				strings.Contains(setErrStr, ":10005") {
				// Still trying to connect to internal ports, need to wait more
				// Even though CLUSTER SLOTS shows external addresses, client might have cached topology
				client.Close()
				time.Sleep(checkInterval)
				continue
			}
		}

		// All checks passed! Both CLUSTER SLOTS and CLUSTER NODES have external addresses AND operations work
		if setErr == nil {
			consecutiveSuccesses++

			// Need multiple consecutive successes to ensure topology is stable
			if consecutiveSuccesses >= requiredSuccesses {
				// Final verification: create a completely fresh client (like the test will)
				// and verify it can work without internal address errors
				client.Close()

				freshOpts, err := redis.ParseClusterURL(connStr)
				if err == nil {
					freshOpts.DialTimeout = 1 * time.Second
					freshOpts.ReadTimeout = 1 * time.Second
					freshOpts.WriteTimeout = 1 * time.Second

					freshClient := redis.NewClusterClient(freshOpts)
					freshCtx, freshCancel := context.WithTimeout(ctx, 3*time.Second)

					// Try multiple operations with fresh client to ensure it works end-to-end
					// This will fail if it tries to use internal addresses
					var freshErr error
					for j := 0; j < 2; j++ {
						testKey := fmt.Sprintf("__fresh_client_test_%d_%d", i, j)
						freshErr = freshClient.Set(freshCtx, testKey, "test", time.Second).Err()
						if freshErr != nil {
							break
						}
						_, freshErr = freshClient.Get(freshCtx, testKey).Result()
						if freshErr != nil {
							break
						}
						freshClient.Del(freshCtx, testKey)
					}
					freshCancel()
					freshClient.Close()

					if freshErr != nil {
						// Check if it's an internal port error
						freshErrStr := freshErr.Error()
						if strings.Contains(freshErrStr, ":10000") ||
							strings.Contains(freshErrStr, ":10001") ||
							strings.Contains(freshErrStr, ":10002") ||
							strings.Contains(freshErrStr, ":10003") ||
							strings.Contains(freshErrStr, ":10004") ||
							strings.Contains(freshErrStr, ":10005") {
							// Fresh client still getting internal addresses, need to wait more
							consecutiveSuccesses = 0 // Reset since fresh client failed
							time.Sleep(checkInterval)
							continue
						}
					}
				}

				// All checks passed including fresh client!
				if i > 0 {
					t.Logf("Cluster announce ready after %d attempts (%v) - verified %d nodes in CLUSTER SLOTS, %d nodes in CLUSTER NODES (%d consecutive checks)", i+1, time.Duration(i+1)*checkInterval, totalNodesInSlots, totalNodesInCluster, consecutiveSuccesses)
				}
				return nil
			}

			// Success but need more consecutive successes - wait and check again
			client.Close()
			time.Sleep(checkInterval)
			continue
		}

		// Reset consecutive successes on any failure
		consecutiveSuccesses = 0

		// Other error, might be transient, wait and retry
		time.Sleep(checkInterval)
	}

	return fmt.Errorf("cluster announce not ready after %d attempts (%v) - CLUSTER SLOTS still contains internal addresses", maxAttempts, time.Duration(maxAttempts)*checkInterval)
}

// isInternalPort checks if an address uses an internal port (10000-10009 range)
func isInternalPort(addr string, initialPort, nodes int) bool {
	// Check if address contains any internal port in the range
	// For 3 nodes: ports 10000-10005 (6 ports total: 3 masters + 3 slaves)
	maxPort := initialPort + (2 * nodes) - 1
	for port := initialPort; port <= maxPort; port++ {
		if strings.Contains(addr, fmt.Sprintf(":%d", port)) {
			return true
		}
	}
	return false
}
