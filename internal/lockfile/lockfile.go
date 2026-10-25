// Package lockfile is the agent's single-instance lock: the file agent.lock in
// its data directory, which the running agent holds under an operating-system
// lock for as long as it runs.
//
// The OS lock, not the file's contents, is what says an agent is running. The
// kernel drops it when the process ends, however it ends, so a lock left by a
// killed agent is free at the next start even when the OS has since given the
// dead agent's PID to another program, and two agents started at the same
// moment cannot both take it. The contents only describe the holder: its PID
// alone on the first line, kept a bare number so any reader can parse it,
// then key=value lines for the web UI's port once known and for the marker
// saying the writer holds the OS lock.
//
// Two cases fall back to asking whether the recorded PID is a running process:
// a file without the marker, written by an agent that did not hold the OS
// lock, and a file system that refuses file locks. A PID alone cannot tell the agent
// from a program that reused its PID, so there the answer leans towards
// "running": the side that never starts a second agent and never moves a data
// directory from under one. Holder.PIDOnly marks those answers, so what the
// user reads can say how to clear a lock that is wrong.
//
// cmd/agent takes and releases the lock. The data-directory migration in
// internal/manager asks LiveHolder whether an agent still runs from a
// directory before moving it. Both go through this package, so they cannot
// disagree about what a live lock is.
package lockfile

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Name is the lock file's name inside the data directory.
const Name = "agent.lock"

const (
	portKey = "port"
	// lockKey=lockValue is written last, so a read that sees it has the whole
	// file, and only by an agent that holds the OS lock.
	lockKey   = "lock"
	lockValue = "os"
)

// Retries. A lock found held is tried again for a moment before it counts:
// the migration's check holds it for the few microseconds it takes, and on
// Windows a program scanning the file can hold it open as briefly. A holder's
// contents are waited for because it takes the lock before writing them.
var (
	heldAttempts    = 10
	contentAttempts = 20
	retryDelay      = 50 * time.Millisecond
)

var (
	// errHeld reports that another open holds the OS lock.
	errHeld = errors.New("the lock is held by another process")
	// errNoOSLock reports that the file system refused the OS lock itself.
	errNoOSLock = errors.New("file locks are not available on this file system")
)

// Content is what a lock file says about the agent that wrote it.
type Content struct {
	PID    int
	Port   int  // 0 until the agent has recorded its web UI's port
	OSLock bool // the writer held the OS lock on the file
}

// Parse reads a lock file's contents. It also reads a port written alone on
// the second line, without a key.
func Parse(data []byte) (Content, bool) {
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	pid, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil || pid <= 0 {
		return Content{}, false
	}
	c := Content{PID: pid}
	for i, line := range lines[1:] {
		line = strings.TrimSpace(line)
		key, value, keyed := strings.Cut(line, "=")
		switch {
		case !keyed && i == 0:
			c.Port = positive(line)
		case key == portKey:
			c.Port = positive(value)
		case key == lockKey:
			c.OSLock = value == lockValue
		}
	}
	return c, true
}

func positive(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

func (c Content) encode() []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "%d\n", c.PID)
	if c.Port > 0 {
		fmt.Fprintf(&b, "%s=%d\n", portKey, c.Port)
	}
	if c.OSLock {
		fmt.Fprintf(&b, "%s=%s\n", lockKey, lockValue)
	}
	return []byte(b.String())
}

// Holder is a running agent that holds a data directory's lock.
type Holder struct {
	// PID is 0 when the holder has not written its PID yet.
	PID int
	// PIDOnly is set when nothing but a running process with the recorded PID
	// says the holder is an agent: the lock has no marker, or the file
	// system refuses file locks. That process may be
	// another program that reused the PID of an agent that died.
	PIDOnly bool
}

// Lock is the lock this agent holds.
type Lock struct {
	path string
	file *os.File // nil when the file system refused the OS lock
	pid  int

	// Stale is the PID named by a lock this one took over, 0 if none.
	Stale int
	// NoOSLock is why the file system refused the OS lock, nil when it is
	// held. Without it a second agent is told apart by its PID alone.
	NoOSLock error
}

// Path is the lock file's path.
func (l *Lock) Path() string { return l.path }

// Acquire takes the lock in dir. When another agent holds it, Acquire returns
// that agent and a nil Lock.
func Acquire(dir string) (*Lock, *Holder, error) {
	path := filepath.Join(dir, Name)
	f, err := openLockedRetrying(path, true)
	switch {
	case errors.Is(err, errHeld):
		return nil, heldBy(path), nil
	case errors.Is(err, errNoOSLock):
		return acquireByPID(path, err)
	case err != nil:
		return nil, nil, err
	}

	data, err := io.ReadAll(f)
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	if h := pidOnlyHolder(data); h != nil {
		_ = f.Close()
		return nil, h, nil
	}
	lock := &Lock{path: path, file: f, pid: os.Getpid()}
	if c, ok := Parse(data); ok {
		lock.Stale = c.PID
	}
	if err := lock.write(Content{PID: lock.pid, OSLock: true}); err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	return lock, nil, nil
}

// acquireByPID is the lock on a file system that refuses file locks: a PID
// found running keeps the lock, anything else is taken over.
func acquireByPID(path string, cause error) (*Lock, *Holder, error) {
	lock := &Lock{path: path, pid: os.Getpid(), NoOSLock: cause}
	if data, err := os.ReadFile(path); err == nil { //nolint:gosec // path is Name inside the agent's own data directory, never external input
		if c, ok := Parse(data); ok {
			if running(c.PID) {
				return nil, &Holder{PID: c.PID, PIDOnly: true}, nil
			}
			lock.Stale = c.PID
		}
	}
	if err := lock.write(Content{PID: lock.pid}); err != nil {
		return nil, nil, err
	}
	return lock, nil, nil
}

// RecordPort adds the web UI's port to the lock, so that a second launch can
// reopen the UI.
func (l *Lock) RecordPort(port int) error {
	if l.file == nil {
		// Without the OS lock another agent may have taken the file over.
		// Its PID must not be overwritten with this agent's port.
		data, err := os.ReadFile(l.path) //nolint:gosec // l.path is Name inside the agent's own data directory, never external input
		if err != nil {
			return err
		}
		if c, ok := Parse(data); !ok || c.PID != l.pid {
			return fmt.Errorf("%s no longer names this agent", l.path)
		}
	}
	return l.write(Content{PID: l.pid, Port: port, OSLock: l.file != nil})
}

func (l *Lock) write(c Content) error {
	if l.file == nil {
		return os.WriteFile(l.path, c.encode(), 0o600)
	}
	if err := l.file.Truncate(0); err != nil {
		return err
	}
	_, err := l.file.WriteAt(c.encode(), 0)
	return err
}

// Release gives the lock up. Under the OS lock the file is emptied and kept:
// removing a file another agent may have open lets that agent lock a file no
// longer in the directory while a third one creates a new file and locks that
// too. An empty file names no holder, for this release and for earlier ones.
// Without the OS lock the file is removed, as long as it still names this
// agent.
func (l *Lock) Release() {
	if l.file != nil {
		_ = l.file.Truncate(0)
		_ = l.file.Close()
		l.file = nil
		return
	}
	if data, err := os.ReadFile(l.path); err == nil { //nolint:gosec // l.path is Name inside the agent's own data directory, never external input
		if c, ok := Parse(data); ok && c.PID == l.pid {
			_ = os.Remove(l.path)
		}
	}
}

// LiveHolder reports the running agent that holds the lock in dir, or nil when
// there is none: no lock file, an empty or unreadable one, or one whose
// writer has ended. It takes the lock only for as long as the check lasts.
func LiveHolder(dir string) *Holder {
	path := filepath.Join(dir, Name)
	f, err := openLockedRetrying(path, false)
	switch {
	case err == nil:
		data, readErr := io.ReadAll(f)
		_ = f.Close()
		if readErr != nil {
			return nil
		}
		return pidOnlyHolder(data)
	case errors.Is(err, errHeld):
		return heldBy(path)
	case errors.Is(err, fs.ErrNotExist):
		return nil
	}
	// No OS lock to ask: the file system refuses it, or the file cannot be
	// opened for writing. The recorded PID is all there is.
	data, err := os.ReadFile(path) //nolint:gosec // path is Name inside an agent data directory, never external input
	if err != nil {
		return nil
	}
	if c, ok := Parse(data); ok && running(c.PID) {
		return &Holder{PID: c.PID, PIDOnly: true}
	}
	return nil
}

// pidOnlyHolder reads contents found with the OS lock free. Contents with the
// marker are stale, whatever their PID now belongs to. Contents without it
// were written by an agent that did not take the OS lock, so a running
// process with their PID is taken for that agent.
func pidOnlyHolder(data []byte) *Holder {
	c, ok := Parse(data)
	if !ok || c.OSLock || !running(c.PID) {
		return nil
	}
	return &Holder{PID: c.PID, PIDOnly: true}
}

// heldBy reads the PID of the agent holding the OS lock on path. The holder
// writes its contents right after taking the lock, so they are waited for.
func heldBy(path string) *Holder {
	for i := 0; i < contentAttempts; i++ {
		if data, err := os.ReadFile(path); err == nil { //nolint:gosec // path is Name inside an agent data directory, never external input
			if c, ok := Parse(data); ok && c.OSLock {
				return &Holder{PID: c.PID}
			}
		}
		time.Sleep(retryDelay)
	}
	return &Holder{}
}

func openLockedRetrying(path string, create bool) (*os.File, error) {
	for i := 1; ; i++ {
		f, err := openLocked(path, create)
		if !errors.Is(err, errHeld) || i >= heldAttempts {
			return f, err
		}
		time.Sleep(retryDelay)
	}
}

// running reports whether pid is a running process other than this one. A
// lock naming this process was left by a dead agent whose PID the OS has
// since handed to it, since this process has not written a lock yet.
func running(pid int) bool {
	return pid != os.Getpid() && ProcessAlive(pid)
}

// ProcessAlive returns true if a process with the given PID is still running.
// Elsewhere signal 0 asks the kernel; Windows has no signal 0, so tasklist is
// asked there.
func ProcessAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		out, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/NH").Output() //nolint:gosec // pid is a numeric int parsed from our own lock file, never external input
		if err != nil {
			return false
		}
		return strings.Contains(string(out), fmt.Sprintf(" %d ", pid))
	}
	// Unix: signal 0 checks existence without harming the process
	return proc.Signal(syscall.Signal(0)) == nil
}
