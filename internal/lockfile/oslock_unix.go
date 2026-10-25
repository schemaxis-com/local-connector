//go:build linux || darwin

package lockfile

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// openLocked opens path for reading and writing and takes an exclusive
// flock(2) on it without waiting. The lock belongs to this open file, which Go
// opens close-on-exec so a browser the agent starts cannot inherit it, and the
// kernel drops it when the process ends, however it ends. flock does not stop
// other processes from reading, so a second agent can still read the holder's
// PID and port.
func openLocked(path string, create bool) (*os.File, error) {
	flag := os.O_RDWR
	if create {
		flag |= os.O_CREATE
	}
	f, err := os.OpenFile(path, flag, 0o600) //nolint:gosec // path is Name inside an agent data directory, never external input
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) //nolint:gosec // a file descriptor always fits in an int
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err == nil {
		return f, nil
	}
	_ = f.Close()
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return nil, errHeld
	}
	// ENOLCK (an NFS mount with no lock service), ENOTSUP and the like: the
	// file system cannot lock, which is not the same as someone holding it.
	return nil, fmt.Errorf("%w: %w", errNoOSLock, err)
}
