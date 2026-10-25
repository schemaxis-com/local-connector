//go:build windows

package lockfile

import (
	"errors"
	"os"
	"syscall"
)

// errorSharingViolation is ERROR_SHARING_VIOLATION, which package syscall
// does not name.
const errorSharingViolation syscall.Errno = 32

// openLocked opens path for reading and writing and shares it for reading
// only. That share mode is the lock: while the agent keeps this handle open,
// every other open that asks to write fails with a sharing violation, and
// Windows closes the handle when the process ends, however it ends. Reading
// stays open to everyone, so a second agent can still read the holder's PID
// and port.
func openLocked(path string, create bool) (*os.File, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	disposition := uint32(syscall.OPEN_EXISTING)
	if create {
		disposition = syscall.OPEN_ALWAYS
	}
	h, err := syscall.CreateFile(name, syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ, nil, disposition, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if errors.Is(err, errorSharingViolation) {
			return nil, errHeld
		}
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}
