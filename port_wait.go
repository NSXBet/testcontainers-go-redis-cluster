package tcredis

import (
	"fmt"
	"net"
	"testing"
	"time"
)

// waitForPortFree polls until a port is actually free (Docker has released the binding)
// This is necessary because Docker's cleanup is async - even after container.Terminate()
// returns successfully, Docker may still be cleaning up network bindings for several seconds
func waitForPortFree(port int, timeout time.Duration, t testing.TB) {
	deadline := time.Now().Add(timeout)
	lastErr := error(nil)

	for time.Now().Before(deadline) {
		// Try to bind to the port
		listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err == nil {
			// Port is free! Close our test listener and return
			listener.Close()
			return
		}

		lastErr = err
		time.Sleep(100 * time.Millisecond)
	}

	// Port still not free after timeout - log warning but don't fail
	// This prevents blocking test execution, but the port won't be reused
	if lastErr != nil {
		t.Logf("Warning: Port %d not free after %v (Docker cleanup still in progress): %v", port, timeout, lastErr)
	}
}
