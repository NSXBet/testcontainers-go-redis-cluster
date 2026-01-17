package tcredis

import "sync"

// portAllocator manages port allocation for parallel tests
// Ports are allocated sequentially starting from a base port, and released ports
// are returned to a free list for reuse
type portAllocator struct {
	mu            sync.Mutex
	nextPort      int
	releasedPorts []int
}

// globalPortAllocator is the shared port allocator for all tests
// Default starting port is 27000 to avoid conflicts with common services
var globalPortAllocator = &portAllocator{
	nextPort:      27000,
	releasedPorts: make([]int, 0),
}

// SetStartingPort sets the starting port for the global port allocator
// This must be called before any tests run (e.g., in TestMain or init)
// Subsequent calls have no effect
//
// Note: For test-specific port configuration, prefer using WithStartingPort() or
// WithStartingPortV2() options instead of this global setting.
func SetStartingPort(port int) {
	globalPortAllocator.mu.Lock()
	defer globalPortAllocator.mu.Unlock()

	// Only set if we haven't allocated any ports yet
	if globalPortAllocator.nextPort == 27000 && len(globalPortAllocator.releasedPorts) == 0 {
		globalPortAllocator.nextPort = port
	}
}

// allocatePort allocates a single port for testing
// Always allocates a new sequential port (no reuse) to avoid Docker async cleanup issues
func (pa *portAllocator) allocatePort() int {
	pa.mu.Lock()
	defer pa.mu.Unlock()

	// Always allocate new sequential port
	// We don't reuse ports because Docker cleanup is async - even after container.Terminate()
	// returns, Docker may still be cleaning up network bindings for several seconds
	// Reusing ports causes "port is already allocated" errors
	port := pa.nextPort
	pa.nextPort++
	return port
}

// releasePort returns a port to the pool for reuse
func (pa *portAllocator) releasePort(port int) {
	pa.mu.Lock()
	defer pa.mu.Unlock()
	pa.releasedPorts = append(pa.releasedPorts, port)
}

// allocatePortRange allocates a contiguous range of ports for multi-node clusters
// Returns the starting port of the allocated range
// Always allocates new sequential ports (no reuse) to avoid Docker async cleanup issues
func (pa *portAllocator) allocatePortRange(count int) int {
	pa.mu.Lock()
	defer pa.mu.Unlock()

	// Always allocate new sequential port range
	// We don't reuse because Docker cleanup is async
	startPort := pa.nextPort
	pa.nextPort += count
	return startPort
}

// releasePortRange returns a range of ports to the pool
func (pa *portAllocator) releasePortRange(startPort, count int) {
	pa.mu.Lock()
	defer pa.mu.Unlock()

	for i := 0; i < count; i++ {
		pa.releasedPorts = append(pa.releasedPorts, startPort+i)
	}
}
