package clone

import (
	"testing"
	"time"
)

// Final review M2: a clone started by the retry endpoint must end well before
// the web's 130 s write timeout.
func TestDefaultCloneTimeoutFitsTheRetryPath(t *testing.T) {
	if DefaultTimeout != 100*time.Second {
		t.Fatalf("DefaultTimeout = %v, want 100s", DefaultTimeout)
	}
}
