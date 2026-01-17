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
// Tries to reuse a released port first, otherwise allocates a new sequential port
// Ports are only released after Docker cleanup completes (verified via waitForPortFree)
func (pa *portAllocator) allocatePort() int {
	pa.mu.Lock()
	defer pa.mu.Unlock()

	// Try to reuse a released port first (these have been verified as free)
	if len(pa.releasedPorts) > 0 {
		port := pa.releasedPorts[len(pa.releasedPorts)-1]
		pa.releasedPorts = pa.releasedPorts[:len(pa.releasedPorts)-1]
		return port
	}

	// No free ports, allocate new one
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
// For ranges, always allocates new sequential ports (ensures contiguous ports)
// Note: Range reuse is not implemented because finding contiguous free ranges is complex
func (pa *portAllocator) allocatePortRange(count int) int {
	pa.mu.Lock()
	defer pa.mu.Unlock()

	// Always allocate new sequential port range
	// Ensuring contiguous ports simplifies cluster setup
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
