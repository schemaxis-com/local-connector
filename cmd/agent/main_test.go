package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/schemaxis-com/local-connector/internal/lockfile"
)

func TestRecordPortAndReadLockPort(t *testing.T) {
	dataDir := t.TempDir()
	lock := acquireLock(dataDir, false)
	defer lock.Release()

	if err := lock.RecordPort(8123); err != nil {
		t.Fatalf("RecordPort: %v", err)
	}

	port, ok := readLockPort(lock.Path())
	if !ok || port != 8123 {
		t.Fatalf("got port=%d ok=%v, want port=8123 ok=true", port, ok)
	}
}

// An agent that is still starting has taken the lock but not recorded its
// port yet.
func TestReadLockPortMissing(t *testing.T) {
	lockPath := seedLock(t, "4242\nlock=os\n")

	if _, ok := readLockPort(lockPath); ok {
		t.Fatal("expected readLockPort to report no port for a lock file without one")
	}
}

// A lock may carry the port alone on the second line, without a key.
func TestReadLockPortWrittenWithoutAKey(t *testing.T) {
	lockPath := seedLock(t, "4242\n8123\n")

	if port, ok := readLockPort(lockPath); !ok || port != 8123 {
		t.Fatalf("got port=%d ok=%v, want port=8123 ok=true", port, ok)
	}
}

// stubBrowser records the URL the code under test opens instead of opening it.
func stubBrowser(t *testing.T) *string {
	t.Helper()
	opened := new(string)
	original := browserOpener
	browserOpener = func(url string) { *opened = url }
	t.Cleanup(func() { browserOpener = original })
	return opened
}

func seedLock(t *testing.T, content string) string {
	t.Helper()
	lockPath := filepath.Join(t.TempDir(), lockfile.Name)
	if err := os.WriteFile(lockPath, []byte(content), 0600); err != nil {
		t.Fatalf("failed to seed lock file: %v", err)
	}
	return lockPath
}

func TestHandleExistingInstanceReopensUI(t *testing.T) {
	lockPath := seedLock(t, "4242\nport=8123\nlock=os\n")
	opened := stubBrowser(t)

	msg := handleExistingInstance(lockPath, &lockfile.Holder{PID: 4242}, true)

	if *opened != "http://localhost:8123" {
		t.Fatalf("got opened URL %q, want http://localhost:8123", *opened)
	}
	if !strings.Contains(msg, "PID 4242") || !strings.Contains(msg, "Reopened the Web UI at http://localhost:8123") {
		t.Fatalf("unexpected message %q", msg)
	}
	if strings.Contains(msg, "delete") {
		t.Fatalf("an agent identified by its OS lock was said to be doubtful: %q", msg)
	}
}

// --no-browser holds on a relaunch too: a script or a service manager that
// starts the agent while one runs must not open a browser on the machine.
func TestHandleExistingInstanceWithNoBrowserOnlyPrintsTheAddress(t *testing.T) {
	lockPath := seedLock(t, "4242\nport=8123\nlock=os\n")
	opened := stubBrowser(t)

	msg := handleExistingInstance(lockPath, &lockfile.Holder{PID: 4242}, false)

	if *opened != "" {
		t.Fatalf("--no-browser opened %q", *opened)
	}
	if !strings.Contains(msg, "Its Web UI is at http://localhost:8123") {
		t.Fatalf("expected the message to give the address, got %q", msg)
	}
}

func TestHandleExistingInstanceWithNoPortYet(t *testing.T) {
	lockPath := seedLock(t, "4242\nlock=os\n")
	opened := stubBrowser(t)

	msg := handleExistingInstance(lockPath, &lockfile.Holder{PID: 4242}, true)

	if *opened != "" {
		t.Fatalf("opened %q with no port recorded", *opened)
	}
	if !strings.Contains(msg, "not recorded its Web UI address yet") {
		t.Fatalf("unexpected message %q", msg)
	}
}

// A holder known only by a running PID may be a program that reused a dead
// agent's PID, so the message says how to clear the lock.
func TestHandleExistingInstanceNamesTheLockWhenOnlyThePIDIsKnown(t *testing.T) {
	lockPath := seedLock(t, "4242\n8123\n")
	stubBrowser(t)

	msg := handleExistingInstance(lockPath, &lockfile.Holder{PID: 4242, PIDOnly: true}, true)

	if !strings.Contains(msg, "If no agent is running as PID 4242, delete "+lockPath) {
		t.Fatalf("the message does not say how to clear the lock: %q", msg)
	}
}

func TestHandleExistingInstanceWithAnUnknownPID(t *testing.T) {
	lockPath := seedLock(t, "4242\nport=8123\nlock=os\n")
	stubBrowser(t)

	msg := handleExistingInstance(lockPath, &lockfile.Holder{}, true)

	if strings.Contains(msg, "PID") {
		t.Fatalf("the message names a PID it does not know: %q", msg)
	}
}

func TestAcquireLockRecoversFromStaleLock(t *testing.T) {
	dataDir := t.TempDir()
	lockPath := filepath.Join(dataDir, lockfile.Name)

	// A PID that is guaranteed not to be alive.
	if err := os.WriteFile(lockPath, []byte("999999\n"), 0600); err != nil {
		t.Fatalf("failed to seed stale lock file: %v", err)
	}

	lock := acquireLock(dataDir, false)
	defer lock.Release()

	data, err := os.ReadFile(lockPath) //nolint:gosec // lockPath is filepath.Join(t.TempDir(), lockfile.Name) in this test, never external input
	if err != nil {
		t.Fatalf("failed to read lock file: %v", err)
	}
	c, ok := lockfile.Parse(data)
	if !ok || c.PID != os.Getpid() {
		t.Fatalf("expected lock to be re-acquired by current process, got %+v ok=%v", c, ok)
	}

	lock.Release()
	if data, err := os.ReadFile(lockPath); err == nil && len(data) != 0 { //nolint:gosec // lockPath is filepath.Join(t.TempDir(), lockfile.Name) in this test, never external input
		t.Fatalf("a released lock still names a holder: %q", data)
	}
}

// A stale lock can carry this process's own PID: the agent that wrote it died
// without removing it and the OS handed its PID to us. The data-directory
// migration also carries a legacy directory's lock over as it is. Reading that
// lock as a live instance would make the agent open its own not-yet-started UI
// and exit; it must take the lock over instead.
func TestAcquireLockTakesOverALockCarryingItsOwnPID(t *testing.T) {
	dataDir := t.TempDir()
	lockPath := filepath.Join(dataDir, lockfile.Name)
	if err := os.WriteFile(lockPath, []byte(strconv.Itoa(os.Getpid())+"\n8123\n"), 0600); err != nil {
		t.Fatalf("failed to seed lock file: %v", err)
	}

	lock := acquireLock(dataDir, false)
	defer lock.Release()

	data, err := os.ReadFile(lockPath) //nolint:gosec // lockPath is filepath.Join(t.TempDir(), lockfile.Name) in this test, never external input
	if err != nil {
		t.Fatalf("failed to read lock file: %v", err)
	}
	if strings.Contains(string(data), "8123") {
		t.Fatalf("the stale lock was kept instead of rewritten: %q", string(data))
	}
}

// A lock left by an agent that died, whose PID now belongs to a running
// program (the parent of the test binary stands in for it), is taken over
// instead of making the agent exit as "already running".
func TestAcquireLockTakesOverALockWhosePIDWasReused(t *testing.T) {
	dataDir := t.TempDir()
	lockPath := filepath.Join(dataDir, lockfile.Name)
	content := strconv.Itoa(os.Getppid()) + "\nport=8123\nlock=os\n"
	if err := os.WriteFile(lockPath, []byte(content), 0600); err != nil {
		t.Fatalf("failed to seed lock file: %v", err)
	}
	opened := stubBrowser(t)

	lock := acquireLock(dataDir, false)
	defer lock.Release()

	if *opened != "" {
		t.Fatalf("the agent took the reused PID for a running agent and opened %q", *opened)
	}
	if lock.Stale != os.Getppid() {
		t.Fatalf("Stale = %d, want %d", lock.Stale, os.Getppid())
	}
}
