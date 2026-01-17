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

	"github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"
	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// RedisV2 sets up a Redis cluster by starting N separate Redis containers
// and configuring them to form a cluster. This approach is faster than
// using the grokzen image since individual Redis containers start very quickly.
// It returns a connection string in the format redis://host:port
// that can be used with Redis clients that support cluster mode.
func RedisV2(t testing.TB, nodes int) string {
	startTime := time.Now()
	if nodes < 3 {
		t.Fatalf("number of nodes must be at least 3 for a Redis cluster, got %d. Redis clusters require at least 3 master nodes to distribute hash slots.", nodes)
	}

	ctx := context.Background()

	// Allocate a port range for this cluster (supports parallel test execution)
	// Each node needs its own port
	initialPort := globalPortAllocator.allocatePortRange(nodes)
	t.Logf("RedisV2 allocated port range %d-%d (%d ports)", initialPort, initialPort+nodes-1, nodes)

	// Register port cleanup - return ports to pool when test completes
	t.Cleanup(func() {
		globalPortAllocator.releasePortRange(initialPort, nodes)
		t.Logf("RedisV2 released port range %d-%d", initialPort, initialPort+nodes-1)
	})

	// Get current directory name for Docker grouping
	currentDir, err := os.Getwd()
	if err != nil {
		t.Logf("Warning: failed to get current directory: %v", err)
		currentDir = "unknown"
	}
	projectName := filepath.Base(currentDir)

	// Use official Redis image
	redisVersion := "7.0.7"
	imageName := fmt.Sprintf("redis:%s", redisVersion)

	// Ensure the image exists locally (pull if needed)
	if err := ensureRedisImage(ctx, t, imageName); err != nil {
		t.Fatalf("Failed to ensure Redis image exists: %v", err)
	}

	// Create a Docker network for the containers to communicate with each other
	networkName := fmt.Sprintf("redis-cluster-v2-%d", time.Now().UnixNano())
	network, err := testcontainers.GenericNetwork(ctx, testcontainers.GenericNetworkRequest{
		NetworkRequest: testcontainers.NetworkRequest{
			Name: networkName,
			Labels: map[string]string{
				"project":        projectName,
				"testcontainers": "true",
				"redis-cluster":  "true",
			},
		},
	})
	if err != nil {
		t.Fatalf("Failed to create Docker network: %v", err)
	}

	// Register network cleanup
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if err := network.Remove(cleanupCtx); err != nil {
			// Fallback to Docker CLI
			cmd := exec.CommandContext(cleanupCtx, "docker", "network", "rm", networkName)
			if err := cmd.Run(); err != nil {
				t.Logf("Warning: Failed to remove network %s: %v", networkName, err)
			}
		}
	})

	// Start N Redis containers
	containers := make([]testcontainers.Container, 0, nodes)
	containerAddrs := make([]string, 0, nodes)      // External addresses (for clients)
	containerIPs := make([]string, 0, nodes)        // Internal IPs (for CLUSTER MEET)

	for i := 0; i < nodes; i++ {
		// Use SAME port inside and outside container (like grokzen approach)
		// This eliminates cluster-announce mismatch issues
		port := initialPort + i // Same port both inside and outside (27000, 27001, 27002...)

		// CRITICAL: Capture port in local variable to avoid closure bug
		// Without this, all containers would use the last value of port!
		portForClosure := port

		req := testcontainers.ContainerRequest{
			Image: imageName,
			// Expose the port
			ExposedPorts: []string{fmt.Sprintf("%d/tcp", port)},
			Networks:     []string{networkName},
			Labels: map[string]string{
				"project":     projectName,
				"testcontainers": "true",
				"redis-cluster": "true",
				"redis-node": fmt.Sprintf("%d", i),
			},
			// Use HostConfigModifier to set FIXED 1:1 port mapping
			// Using same port inside and outside eliminates announce propagation issues
			HostConfigModifier: func(hostConfig *container.HostConfig) {
				hostConfig.PortBindings = nat.PortMap{
					nat.Port(fmt.Sprintf("%d/tcp", portForClosure)): []nat.PortBinding{
						{
							HostIP:   "0.0.0.0",
							HostPort: fmt.Sprintf("%d", portForClosure),
						},
					},
				}
			},
			// Configure Redis via command-line arguments
			Cmd: []string{
				"redis-server",
				"--port", fmt.Sprintf("%d", port),
				"--cluster-enabled", "yes",
				"--cluster-config-file", fmt.Sprintf("nodes-%d.conf", port),
				"--cluster-node-timeout", "2000",
				"--bind", "0.0.0.0",
				"--cluster-announce-ip", "127.0.0.1",
				"--cluster-announce-port", fmt.Sprintf("%d", port),
			},
			WaitingFor: wait.ForListeningPort(nat.Port(fmt.Sprintf("%d/tcp", port))).
				WithStartupTimeout(10 * time.Second),
		}

		redisC, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
			ContainerRequest: req,
			Started:          true,
		})
		if err != nil {
			// Cleanup already started containers
			for _, c := range containers {
				c.Terminate(ctx)
			}
			t.Fatalf("Failed to start Redis container %d: %v", i, err)
		}

		// Register cleanup - capture container in local scope to avoid closure bug
		// This is critical: without this, all cleanups would reference the last container
		container := redisC
		nodeIndex := i
		t.Cleanup(func() {
			cleanupCtx := context.Background()
			if err := container.Terminate(cleanupCtx); err != nil {
				t.Logf("Failed to terminate Redis container %d: %v", nodeIndex, err)
			}
		})

		containers = append(containers, redisC)
		if i == 0 {
			t.Logf("First container started in %v", time.Since(startTime))
		}
		if i == nodes-1 {
			t.Logf("All %d containers started in %v", nodes, time.Since(startTime))
		}

		// Get host (external address for clients)
		host, err := redisC.Host(ctx)
		if err != nil {
			t.Fatalf("Failed to get host for container %d: %v", i, err)
		}
		if host == "localhost" {
			host = "127.0.0.1"
		}

		// We're using 1:1 port mappings, so port is same inside and outside
		containerAddrs = append(containerAddrs, fmt.Sprintf("%s:%d", host, port))

		// Get internal container IP for CLUSTER MEET
		containerIP, err := redisC.ContainerIP(ctx)
		if err != nil {
			t.Fatalf("Failed to get container IP for container %d: %v", i, err)
		}
		containerIPs = append(containerIPs, containerIP)
	}

	// Wait a moment for all nodes to fully start before cluster creation
	time.Sleep(500 * time.Millisecond)

	// Form cluster manually using CLUSTER MEET (internal IPs) and CLUSTER ADDSLOTS
	// This is more reliable than redis-cli --cluster create which hangs

	// Step 1: Full mesh topology - have ALL nodes meet ALL other nodes
	// This is critical for gossip protocol to work properly
	for i := 0; i < nodes; i++ {
		for j := 0; j < nodes; j++ {
			if i == j {
				continue
			}
			portI := initialPort + i
			portJ := initialPort + j

			exitCode, _, err := containers[i].Exec(ctx, []string{
				"redis-cli", "-p", fmt.Sprintf("%d", portI),
				"CLUSTER", "MEET", containerIPs[j], fmt.Sprintf("%d", portJ),
			})
			if err != nil || exitCode != 0 {
				t.Logf("Warning: Node %d failed to meet node %d (may be OK if already met): exitCode=%d, err=%v", i, j, exitCode, err)
			}
		}
	}

	// Brief wait for initial gossip propagation
	// The waitForClusterAnnounceReadyV2 will poll until all nodes are present
	time.Sleep(1 * time.Second)

	// Step 2: Assign hash slots evenly across nodes
	slotsPerNode := 16384 / nodes
	for i := 0; i < nodes; i++ {
		startSlot := i * slotsPerNode
		endSlot := startSlot + slotsPerNode - 1
		if i == nodes-1 {
			endSlot = 16383
		}

		port := initialPort + i
		var slotArgs []string
		for slot := startSlot; slot <= endSlot; slot++ {
			slotArgs = append(slotArgs, fmt.Sprintf("%d", slot))
		}

		addSlotsCmd := append([]string{"redis-cli", "-p", fmt.Sprintf("%d", port), "CLUSTER", "ADDSLOTS"}, slotArgs...)
		exitCode, _, err := containers[i].Exec(ctx, addSlotsCmd)
		if err != nil || exitCode != 0 {
			t.Fatalf("Failed to add slots to node %d: exitCode=%d, err=%v", i, exitCode, err)
		}
	}

	// Wait for slot assignment to propagate
	time.Sleep(500 * time.Millisecond)

	clusterCreateTime := time.Since(startTime)
	t.Logf("Cluster created successfully in %v (total: %v)", clusterCreateTime, clusterCreateTime)

	// Wait for cluster to be ready (cluster creation already did this, but verify quickly)
	readyStartTime := time.Now()
	if err := waitForClusterReadyV2(ctx, t, containerAddrs[0], nodes); err != nil {
		t.Fatalf("Cluster failed to become ready: %v", err)
	}
	t.Logf("Cluster ready check took %v (total: %v)", time.Since(readyStartTime), time.Since(startTime))

	// Verify cluster is ready for ClusterClient operations
	// This ensures tests can use the cluster immediately without custom dialers
	announceStartTime := time.Now()
	connStr := fmt.Sprintf("redis://%s", containerAddrs[0])
	if len(containerAddrs) > 1 {
		connStr = fmt.Sprintf("redis://%s?%s", containerAddrs[0], buildAddrParams(containerAddrs[1:]))
	}
	if err := waitForClusterClientReady(ctx, t, connStr); err != nil {
		t.Fatalf("Cluster failed ClusterClient readiness check: %v", err)
	}
	t.Logf("Cluster ready for clients in %v (total: %v)", time.Since(announceStartTime), time.Since(startTime))

	totalTime := time.Since(startTime)
	t.Logf("RedisV2 total time: %v", totalTime)

	// Return connection string with all master addresses
	if len(containerAddrs) > 1 {
		return fmt.Sprintf("redis://%s?%s", containerAddrs[0], buildAddrParams(containerAddrs[1:]))
	}
	return fmt.Sprintf("redis://%s", containerAddrs[0])
}

// ensureRedisImage checks if the Redis image exists locally, pulls it if needed
func ensureRedisImage(ctx context.Context, t testing.TB, imageName string) error {
	cmd := exec.CommandContext(ctx, "docker", "image", "inspect", imageName)
	if err := cmd.Run(); err == nil {
		// Image exists, we're good
		return nil
	}

	// Image doesn't exist, pull it
	t.Logf("Image %s not found locally, pulling it...", imageName)
	cmd = exec.CommandContext(ctx, "docker", "pull", imageName)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to pull image: %w", err)
	}

	t.Logf("✅ Successfully pulled image: %s", imageName)
	return nil
}

// waitForClusterReadyV2 waits for the cluster to be ready (hash slots assigned)
func waitForClusterReadyV2(ctx context.Context, t testing.TB, firstAddr string, nodes int) error {
	maxAttempts := 30
	checkInterval := 200 * time.Millisecond

	// Use a regular client (not cluster client) to check cluster info
	client := redis.NewClient(&redis.Options{
		Addr:         firstAddr,
		DialTimeout:  500 * time.Millisecond, // Faster timeouts
		ReadTimeout:  500 * time.Millisecond,
		WriteTimeout: 500 * time.Millisecond,
	})
	defer client.Close()

	for i := 0; i < maxAttempts; i++ {
		// Check cluster info directly - ping is redundant
		checkCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond) // Faster timeout
		info, err := client.ClusterInfo(checkCtx).Result()
		cancel()

		if err == nil {
			// Check if cluster is ready
			if strings.Contains(info, "cluster_state:ok") {
				// Check slots assigned
				if strings.Contains(info, "cluster_slots_assigned:16384") {
					if i > 0 {
						t.Logf("Cluster ready after %d attempts", i+1)
					}
					return nil
				}
			}
		}

		time.Sleep(checkInterval)
	}

	return fmt.Errorf("cluster not ready after %d attempts", maxAttempts)
}

// waitForClusterClientReady verifies the cluster works with ClusterClient (not just regular client)
// This ensures tests can use the cluster immediately without hitting MOVED redirect issues
func waitForClusterClientReady(ctx context.Context, t testing.TB, connStr string) error {
	maxAttempts := 200 // Allow up to 20 seconds for cluster client to work reliably
	checkInterval := 100 * time.Millisecond

	for i := 0; i < maxAttempts; i++ {
		opts, err := redis.ParseClusterURL(connStr)
		if err != nil {
			return err
		}

		opts.DialTimeout = 1 * time.Second
		opts.ReadTimeout = 1 * time.Second
		opts.WriteTimeout = 1 * time.Second

		client := redis.NewClusterClient(opts)

		// Try a SET operation with the cluster client
		// This will fail if cluster topology isn't ready or if CLUSTER SLOTS has unreachable IPs
		checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		testKey := fmt.Sprintf("__cluster_ready_test_%d", i)
		setErr := client.Set(checkCtx, testKey, "test", time.Second).Err()

		// If successful, verify GET works too
		var getErr error
		if setErr == nil {
			_, getErr = client.Get(checkCtx, testKey).Result()
			if getErr == nil {
				client.Del(checkCtx, testKey)
			}
		}
		cancel()
		client.Close()

		if setErr == nil && getErr == nil {
			if i > 0 {
				t.Logf("ClusterClient ready after %d attempts (%v)", i+1, time.Duration(i+1)*checkInterval)
			}
			return nil
		}

		// If we got internal IP connection errors, wait longer for announce to propagate
		if setErr != nil && (strings.Contains(setErr.Error(), "172.") || strings.Contains(setErr.Error(), "10.")) {
			time.Sleep(checkInterval * 2) // Wait longer when seeing internal IP errors
			continue
		}

		time.Sleep(checkInterval)
	}

	return fmt.Errorf("ClusterClient not ready after %d attempts (%v)", maxAttempts, time.Duration(maxAttempts)*checkInterval)
}


