package tcredis

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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
	initialPort := 7000 // Start from 7000 for cluster mode

	// Get current directory name for Docker grouping
	currentDir, err := os.Getwd()
	if err != nil {
		t.Logf("Warning: failed to get current directory: %v", err)
		currentDir = "unknown"
	}
	projectName := filepath.Base(currentDir)

	// Ensure local Redis image exists (we'll use official redis image)
	redisVersion := "7.0.7"
	imageName := fmt.Sprintf("redis:%s", redisVersion)
	if err := ensureRedisImage(ctx, t, imageName); err != nil {
		t.Fatalf("Failed to ensure Redis image exists: %v", err)
	}

	// Create a Docker network for the containers to communicate
	networkName := fmt.Sprintf("redis-cluster-v2-%d", time.Now().UnixNano())
	network, err := testcontainers.GenericNetwork(ctx, testcontainers.GenericNetworkRequest{
		NetworkRequest: testcontainers.NetworkRequest{
			Name: networkName,
			Labels: map[string]string{
				"project":     projectName,
				"testcontainers": "true",
				"redis-cluster": "true",
			},
		},
	})
	if err != nil {
		t.Fatalf("Failed to create Docker network: %v", err)
	}

	// Register network cleanup
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		if err := network.Remove(cleanupCtx); err != nil {
			t.Logf("Failed to remove network: %v", err)
		}
	})

	// Start N Redis containers
	containers := make([]testcontainers.Container, 0, nodes)
	containerAddrs := make([]string, 0, nodes)      // External addresses (for clients)
	internalAddrs := make([]string, 0, nodes)       // Internal addresses (for cluster create)

	// Get host IP for cluster configuration
	// We'll use the first container's host as the base
	var baseHost string

	for i := 0; i < nodes; i++ {
		port := initialPort + i
		req := testcontainers.ContainerRequest{
			Image: imageName,
			ExposedPorts: []string{fmt.Sprintf("%d/tcp", port)},
			Networks: []string{networkName},
			Labels: map[string]string{
				"project":     projectName,
				"testcontainers": "true",
				"redis-cluster": "true",
				"redis-node": fmt.Sprintf("%d", i),
			},
			WaitingFor: wait.ForListeningPort(nat.Port(fmt.Sprintf("%d/tcp", port))).
				WithStartupTimeout(10 * time.Second),
			Cmd: []string{
				"redis-server",
				"--port", fmt.Sprintf("%d", port),
				"--cluster-enabled", "yes",
				"--cluster-config-file", fmt.Sprintf("nodes-%d.conf", port),
				"--cluster-node-timeout", "2000", // Reduced from 5000ms for faster startup
				// Skip AOF for test containers - no persistence needed, faster startup
				"--bind", "0.0.0.0",
			},
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

		// Get host and mapped port (external address for clients)
		host, err := redisC.Host(ctx)
		if err != nil {
			t.Fatalf("Failed to get host for container %d: %v", i, err)
		}
		if host == "localhost" {
			host = "127.0.0.1"
		}

		if i == 0 {
			baseHost = host
		}

		mappedPort, err := redisC.MappedPort(ctx, nat.Port(fmt.Sprintf("%d/tcp", port)))
		if err != nil {
			t.Fatalf("Failed to get mapped port for container %d: %v", i, err)
		}

		containerAddrs = append(containerAddrs, fmt.Sprintf("%s:%s", host, mappedPort.Port()))

		// Get internal container IP and port for cluster creation
		// We need to use the container's internal network to create the cluster
		containerIP, err := redisC.ContainerIP(ctx)
		if err != nil {
			t.Fatalf("Failed to get container IP for container %d: %v", i, err)
		}
		internalAddrs = append(internalAddrs, fmt.Sprintf("%s:%d", containerIP, port))
	}

	// Create the cluster using redis-cli --cluster create
	// We use internal addresses for cluster creation (containers can talk to each other)
	// Then we configure announce addresses so clients can connect from outside
	port := initialPort
	createCmd := []string{"redis-cli", "-p", fmt.Sprintf("%d", port), "--cluster", "create"}
	for _, addr := range internalAddrs {
		createCmd = append(createCmd, addr)
	}
	createCmd = append(createCmd, "--cluster-replicas", "0")

	// Execute cluster create - need to pipe "yes" for confirmation
	// Use sh -c to pipe yes into the command
	createCmdStr := fmt.Sprintf("echo yes | %s", strings.Join(createCmd, " "))
	exitCode, output, err := containers[0].Exec(ctx, []string{"sh", "-c", createCmdStr})
	if err != nil || exitCode != 0 {
		t.Logf("Cluster create output: %v", output)
		t.Fatalf("Failed to create cluster: exitCode=%d, err=%v. Command: %s", exitCode, err, createCmdStr)
	}
	clusterCreateTime := time.Since(startTime)
	t.Logf("Cluster created successfully in %v (total: %v)", clusterCreateTime, clusterCreateTime)

	// Configure cluster-announce-ip and cluster-announce-port immediately after cluster creation
	// This must be done AFTER creating the cluster
	announceConfigStart := time.Now()
	if err := configureClusterAnnounceV2(ctx, t, containers, initialPort, baseHost); err != nil {
		t.Logf("Warning: some cluster announce configuration failed: %v", err)
	}
	t.Logf("Cluster announce configuration took %v", time.Since(announceConfigStart))

	// Wait for cluster to be ready (cluster creation already did this, but verify quickly)
	readyStartTime := time.Now()
	if err := waitForClusterReadyV2(ctx, t, containerAddrs[0], nodes); err != nil {
		t.Fatalf("Cluster failed to become ready: %v", err)
	}
	t.Logf("Cluster ready check took %v (total: %v)", time.Since(readyStartTime), time.Since(startTime))

	// Verify cluster announce has propagated
	announceStartTime := time.Now()
	if err := waitForClusterAnnounceReadyV2(ctx, t, containerAddrs, initialPort, nodes); err != nil {
		t.Logf("Warning: cluster announce may not be fully ready: %v (continuing anyway)", err)
	}
	t.Logf("Cluster announce check took %v (total: %v)", time.Since(announceStartTime), time.Since(startTime))

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

// configureClusterAnnounceV2 configures cluster-announce-ip and cluster-announce-port on all nodes in parallel
func configureClusterAnnounceV2(ctx context.Context, t testing.TB, containers []testcontainers.Container, initialPort int, baseHost string) error {
	type configResult struct {
		index int
		err   error
	}

	results := make(chan configResult, len(containers))
	var wg sync.WaitGroup

	// Configure all nodes in parallel
	for i, container := range containers {
		wg.Add(1)
		go func(idx int, cont testcontainers.Container) {
			defer wg.Done()

			port := initialPort + idx
			mappedPort, err := cont.MappedPort(ctx, nat.Port(fmt.Sprintf("%d/tcp", port)))
			if err != nil {
				results <- configResult{index: idx, err: fmt.Errorf("failed to get mapped port: %w", err)}
				return
			}

			// Configure cluster-announce-ip
			_, _, err = cont.Exec(ctx, []string{
				"redis-cli",
				"-p", fmt.Sprintf("%d", port),
				"CONFIG", "SET", "cluster-announce-ip", baseHost,
			})
			if err != nil {
				results <- configResult{index: idx, err: fmt.Errorf("failed to set cluster-announce-ip: %w", err)}
				return
			}

			// Configure cluster-announce-port
			_, _, err = cont.Exec(ctx, []string{
				"redis-cli",
				"-p", fmt.Sprintf("%d", port),
				"CONFIG", "SET", "cluster-announce-port", mappedPort.Port(),
			})
			if err != nil {
				results <- configResult{index: idx, err: fmt.Errorf("failed to set cluster-announce-port: %w", err)}
				return
			}

			results <- configResult{index: idx, err: nil}
		}(i, container)
	}

	wg.Wait()
	close(results)

	var errs []error
	for result := range results {
		if result.err != nil {
			errs = append(errs, fmt.Errorf("node %d: %w", result.index, result.err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("configuration errors: %v", errs)
	}
	return nil
}

// waitForClusterAnnounceReadyV2 polls to verify that cluster announce configuration has propagated
// Ultra-fast version: only checks CLUSTER SLOTS (what clients actually use) and does a single SET/GET
func waitForClusterAnnounceReadyV2(ctx context.Context, t testing.TB, addrs []string, initialPort, nodes int) error {
	maxAttempts := 15 // Reduced - if not ready in 750ms, something is wrong
	checkInterval := 50 * time.Millisecond // Faster polling

	// No initial delay - start checking immediately (announce config happens in parallel)

	connStr := fmt.Sprintf("redis://%s", addrs[0])
	if len(addrs) > 1 {
		connStr = fmt.Sprintf("redis://%s?%s", addrs[0], buildAddrParams(addrs[1:]))
	}

	// Single success is enough - CLUSTER SLOTS is authoritative
	consecutiveSuccesses := 0
	requiredSuccesses := 1 // Single check is sufficient

	for i := 0; i < maxAttempts; i++ {
		opts, err := redis.ParseClusterURL(connStr)
		if err != nil {
			time.Sleep(checkInterval)
			continue
		}

		opts.DialTimeout = 300 * time.Millisecond // Very fast timeouts
		opts.ReadTimeout = 300 * time.Millisecond
		opts.WriteTimeout = 300 * time.Millisecond

		client := redis.NewClusterClient(opts)

		// Get CLUSTER SLOTS - this is the authoritative source for client routing
		// Skip CLUSTER NODES check - SLOTS is what clients actually use
		checkCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond) // Very fast
		slots, err := client.ClusterSlots(checkCtx).Result()
		cancel()

		if err != nil {
			errStr := err.Error()
			// Check if error contains any internal port
			hasInternalPort := false
			for port := initialPort; port < initialPort+nodes; port++ {
				if strings.Contains(errStr, fmt.Sprintf(":%d", port)) {
					hasInternalPort = true
					break
				}
			}

			client.Close()

			if hasInternalPort {
				time.Sleep(checkInterval)
				continue
			}

			// Other error, might be transient
			time.Sleep(checkInterval)
			continue
		}

		// Verify ALL addresses in CLUSTER SLOTS are external
		// Only check CLUSTER SLOTS - this is what clients actually use, so it's sufficient
		hasInternalPortInSlots := false
		totalNodesInSlots := 0

		for _, slot := range slots {
			// Check master node (first node)
			if len(slot.Nodes) > 0 && slot.Nodes[0].Addr != "" {
				totalNodesInSlots++
				addr := slot.Nodes[0].Addr
				if isInternalPortV2(addr, initialPort, nodes) {
					hasInternalPortInSlots = true
					break
				}
			}

			// Check replica nodes (remaining nodes in slot)
			for j := 1; j < len(slot.Nodes); j++ {
				if slot.Nodes[j].Addr != "" {
					totalNodesInSlots++
					addr := slot.Nodes[j].Addr
					if isInternalPortV2(addr, initialPort, nodes) {
						hasInternalPortInSlots = true
						break
					}
				}
			}

			if hasInternalPortInSlots {
				break
			}
		}

		if hasInternalPortInSlots {
			client.Close()
			time.Sleep(checkInterval)
			continue
		}

		// All addresses are external! Now verify operations work
		// Force reload to ensure client uses the verified topology
		client.ReloadState(ctx)

		// Single SET/GET operation is sufficient - if CLUSTER SLOTS shows external addresses
		// and one operation works, the cluster is ready
		checkCtx, cancel = context.WithTimeout(ctx, 1*time.Second) // Fast timeout
		testKey := fmt.Sprintf("__cluster_announce_test_%d", i)
		setErr := client.Set(checkCtx, testKey, "test", 500*time.Millisecond).Err()
		if setErr == nil {
			_, setErr = client.Get(checkCtx, testKey).Result()
			if setErr == nil {
				client.Del(checkCtx, testKey)
			}
		}
		cancel()

		// Check if SET/GET error is about internal addresses
		if setErr != nil {
			errStr := setErr.Error()
			hasInternalPortInError := false
			for port := initialPort; port < initialPort+nodes; port++ {
				if strings.Contains(errStr, fmt.Sprintf(":%d", port)) {
					hasInternalPortInError = true
					break
				}
			}

			if hasInternalPortInError {
				// Still trying to connect to internal ports, need to wait more
				client.Close()
				time.Sleep(checkInterval)
				continue
			}
		}

		// All checks passed! CLUSTER SLOTS has external addresses AND operations work
		if setErr == nil {
			consecutiveSuccesses++

			// Single success is enough - CLUSTER SLOTS is authoritative
			if consecutiveSuccesses >= requiredSuccesses {
				// Skip fresh client check - if CLUSTER SLOTS shows external addresses
				// and SET/GET works, that's sufficient. Fresh client will work the same way.
				client.Close()

				if i > 0 {
					t.Logf("Cluster announce ready after %d attempts (%v) - verified %d nodes in CLUSTER SLOTS", i+1, time.Duration(i+1)*checkInterval, totalNodesInSlots)
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
		client.Close()
		time.Sleep(checkInterval)
	}

	return fmt.Errorf("cluster announce not ready after %d attempts (%v) - CLUSTER SLOTS still contains internal addresses", maxAttempts, time.Duration(maxAttempts)*checkInterval)
}

// isInternalPortV2 checks if an address uses an internal port
func isInternalPortV2(addr string, initialPort, nodes int) bool {
	maxPort := initialPort + nodes - 1
	for port := initialPort; port <= maxPort; port++ {
		if strings.Contains(addr, fmt.Sprintf(":%d", port)) {
			return true
		}
	}
	return false
}

