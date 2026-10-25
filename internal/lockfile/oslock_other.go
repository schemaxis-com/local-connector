//go:build !windows && !linux && !darwin

package lockfile

import (
	"fmt"
	"os"
)

// openLocked has no OS lock to take on the platforms the agent is not
// released for, so the lock falls back to the recorded PID there.
func openLocked(string, bool) (*os.File, error) {
	return nil, fmt.Errorf("%w on this platform", errNoOSLock)
}
