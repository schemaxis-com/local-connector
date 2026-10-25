package lockfile

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// deadPID is large enough to be extremely unlikely to be in use on any platform.
const deadPID = 999999

// holdEnv makes the test binary a helper process that holds the lock in the
// directory it names until its standard input closes or it is killed.
const holdEnv = "LOCKFILE_TEST_HOLD_DIR"

func TestMain(m *testing.M) {
	if dir := os.Getenv(holdEnv); dir != "" {
		lock, holder, err := Acquire(dir)
		if err != nil || holder != nil {
			fmt.Printf("failed %v %v\n", err, holder)
			os.Exit(1)
		}
		fmt.Printf("held %d\n", os.Getpid())
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		lock.Release()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// startHolder starts a separate process holding the lock in dir: a running
// agent, as far as this package can tell.
func startHolder(t *testing.T, dir string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0]) //nolint:gosec // the test binary itself, re-run as a helper
	cmd.Env = append(os.Environ(), holdEnv+"="+dir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "held ") {
		t.Fatalf("the helper did not take the lock: %q %v", line, err)
	}
	return cmd
}

func writeLock(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, Name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readLock(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, Name)) //nolint:gosec // dir is a t.TempDir() path in this test, never external input
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func acquire(t *testing.T, dir string) *Lock {
	t.Helper()
	lock, holder, err := Acquire(dir)
	if err != nil || holder != nil {
		t.Fatalf("Acquire = (%v, %+v, %v), want the lock", lock, holder, err)
	}
	t.Cleanup(lock.Release)
	return lock
}

func TestProcessAlive(t *testing.T) {
	if !ProcessAlive(os.Getpid()) {
		t.Fatal("expected own process to be reported alive")
	}
	if ProcessAlive(deadPID) {
		t.Fatal("expected a clearly nonexistent PID to be reported dead")
	}
}

func TestParse(t *testing.T) {
	cases := []struct {
		name, data string
		want       Content
		ok         bool
	}{
		{"PID only", "1234\n", Content{PID: 1234}, true},
		{"PID and a bare port", "1234\n8088\n", Content{PID: 1234, Port: 8088}, true},
		{"PID, port and marker", "1234\nport=8088\nlock=os\n", Content{PID: 1234, Port: 8088, OSLock: true}, true},
		{"marker before the port is known", "1234\nlock=os\n", Content{PID: 1234, OSLock: true}, true},
		{"Windows line endings", "1234\r\nport=8088\r\nlock=os\r\n", Content{PID: 1234, Port: 8088, OSLock: true}, true},
		{"an unknown key from a later release", "1234\nlock=os\nhost=x\n", Content{PID: 1234, OSLock: true}, true},
		{"an unknown lock kind", "1234\nlock=other\n", Content{PID: 1234}, true},
		{"a bad port", "1234\nport=-1\n", Content{PID: 1234}, true},
		{"empty", "", Content{}, false},
		{"garbage", "not-a-pid\n", Content{}, false},
		{"a PID of zero", "0\n", Content{}, false},
		{"a negative PID", "-5\n", Content{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Parse([]byte(tc.data))
			if ok != tc.ok || got != tc.want {
				t.Fatalf("Parse(%q) = (%+v, %v), want (%+v, %v)", tc.data, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// A reader that takes the first line as the PID must still find a bare PID
// there, or it would take a running agent's lock for garbage and start a
// second agent next to it.
func TestEncodeKeepsTheFirstLineABarePID(t *testing.T) {
	c := Content{PID: 4242, Port: 8088, OSLock: true}
	data := c.encode()
	if first, _, _ := strings.Cut(string(data), "\n"); first != "4242" {
		t.Fatalf("first line %q, want the bare PID", first)
	}
	if got, ok := Parse(data); !ok || got != c {
		t.Fatalf("Parse(encode(%+v)) = (%+v, %v)", c, got, ok)
	}
}

func TestAcquireRecordPortRelease(t *testing.T) {
	dir := t.TempDir()
	lock, holder, err := Acquire(dir)
	if err != nil || holder != nil {
		t.Fatalf("Acquire = (%v, %+v, %v)", lock, holder, err)
	}
	if lock.NoOSLock != nil {
		t.Fatalf("the OS lock was refused on the test machine's temp directory: %v", lock.NoOSLock)
	}
	if lock.Stale != 0 {
		t.Errorf("Stale = %d on a directory with no lock", lock.Stale)
	}

	if err := lock.RecordPort(8123); err != nil {
		t.Fatal(err)
	}
	want := Content{PID: os.Getpid(), Port: 8123, OSLock: true}
	if got, ok := Parse([]byte(readLock(t, dir))); !ok || got != want {
		t.Fatalf("lock contents %+v, want %+v", got, want)
	}

	lock.Release()
	if got := readLock(t, dir); got != "" {
		t.Errorf("a released lock still names a holder: %q", got)
	}
	again := acquire(t, dir)
	if again.Stale != 0 {
		t.Errorf("a released lock was reported stale (PID %d)", again.Stale)
	}
}

// The kernel property everything rests on, with a real second process: while
// it runs, its lock is seen with its PID; once it is killed, which runs none
// of its cleanup, the lock is free.
func TestALockHeldByAnotherProcessIsFreedWhenItIsKilled(t *testing.T) {
	dir := t.TempDir()
	cmd := startHolder(t, dir)
	childPID := cmd.Process.Pid

	if lock, holder, err := Acquire(dir); err != nil || lock != nil || holder == nil || holder.PID != childPID || holder.PIDOnly {
		t.Fatalf("Acquire = (%v, %+v, %v), want held by PID %d", lock, holder, err, childPID)
	}
	if h := LiveHolder(dir); h == nil || h.PID != childPID || h.PIDOnly {
		t.Fatalf("LiveHolder = %+v, want PID %d", h, childPID)
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	if h := LiveHolder(dir); h != nil {
		t.Fatalf("LiveHolder = %+v after the holder was killed", h)
	}
	lock := acquire(t, dir)
	if lock.Stale != childPID {
		t.Errorf("Stale = %d, want the killed holder's PID %d", lock.Stale, childPID)
	}
}

// Two agents started at the same moment: exactly one gets the lock.
func TestASecondOpenInTheSameProcessIsRefused(t *testing.T) {
	dir := t.TempDir()
	acquire(t, dir)
	lock, holder, err := Acquire(dir)
	if err != nil || lock != nil || holder == nil || holder.PID != os.Getpid() {
		t.Fatalf("Acquire = (%v, %+v, %v), want held by this process", lock, holder, err)
	}
}

// An agent died without its cleanup and the OS gave its PID to another
// program, which is running. The OS lock is free, so the lock is stale
// whatever that PID now is. The parent of the test binary stands in for that
// program: a live process other than this one.
func TestALockWhosePIDWasReusedIsStale(t *testing.T) {
	dir := t.TempDir()
	reused := os.Getppid()
	writeLock(t, dir, fmt.Sprintf("%d\nport=8088\nlock=os\n", reused))

	if h := LiveHolder(dir); h != nil {
		t.Fatalf("LiveHolder = %+v, a program that reused the PID was taken for an agent", h)
	}
	lock := acquire(t, dir)
	if lock.Stale != reused {
		t.Errorf("Stale = %d, want %d", lock.Stale, reused)
	}
	if got, _ := Parse([]byte(readLock(t, dir))); got.PID != os.Getpid() || !got.OSLock {
		t.Errorf("the lock was not rewritten for this agent: %+v", got)
	}
}

// A lock without the marker was written by an agent that did not take the OS
// lock, so a running PID in it is all there is to go on, and it is taken for that agent: starting next to it
// would put two agents on the same connection tokens.
func TestALockWithoutTheMarkerAndARunningPIDIsKept(t *testing.T) {
	dir := t.TempDir()
	running := os.Getppid()
	content := fmt.Sprintf("%d\n8088\n", running)
	writeLock(t, dir, content)

	lock, holder, err := Acquire(dir)
	if err != nil || lock != nil || holder == nil || holder.PID != running || !holder.PIDOnly {
		t.Fatalf("Acquire = (%v, %+v, %v), want held by PID %d, by PID only", lock, holder, err, running)
	}
	if got := readLock(t, dir); got != content {
		t.Errorf("the lock was changed to %q", got)
	}
	if h := LiveHolder(dir); h == nil || h.PID != running || !h.PIDOnly {
		t.Errorf("LiveHolder = %+v, want PID %d by PID only", h, running)
	}
}

func TestStaleLocksOfAnEarlierReleaseAreTakenOver(t *testing.T) {
	for name, pid := range map[string]int{
		"a dead PID": deadPID,
		// This process has not written a lock yet, so one naming it was left
		// by a dead agent whose PID the OS handed to this process.
		"this process's own PID": os.Getpid(),
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeLock(t, dir, strconv.Itoa(pid)+"\n8088\n")
			if h := LiveHolder(dir); h != nil {
				t.Fatalf("LiveHolder = %+v", h)
			}
			if lock := acquire(t, dir); lock.Stale != pid {
				t.Errorf("Stale = %d, want %d", lock.Stale, pid)
			}
		})
	}
}

func TestLiveHolderWithNoLock(t *testing.T) {
	dir := t.TempDir()
	if h := LiveHolder(dir); h != nil {
		t.Fatalf("LiveHolder = %+v on a directory with no lock file", h)
	}
	if _, err := os.Stat(filepath.Join(dir, Name)); !os.IsNotExist(err) {
		t.Errorf("LiveHolder created a lock file (err=%v)", err)
	}
	writeLock(t, dir, "")
	if h := LiveHolder(dir); h != nil {
		t.Fatalf("LiveHolder = %+v on an empty lock file", h)
	}
}

// A lock found held counts only once it has stayed held for the retry window,
// so the migration's check, which holds a lock for microseconds, never makes
// an agent starting at that moment take itself for a second one.
func TestABrieflyHeldLockIsWaitedFor(t *testing.T) {
	dir := t.TempDir()
	f, err := openLocked(filepath.Join(dir, Name), true)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(3 * retryDelay)
		_ = f.Close()
	}()
	acquire(t, dir)
}

// On a file system that refuses file locks the lock falls back to the PID.
func TestThePIDFallback(t *testing.T) {
	cause := fmt.Errorf("%w: test", errNoOSLock)

	t.Run("a running PID keeps the lock", func(t *testing.T) {
		dir := t.TempDir()
		writeLock(t, dir, fmt.Sprintf("%d\nport=8088\nlock=os\n", os.Getppid()))
		lock, holder, err := acquireByPID(filepath.Join(dir, Name), cause)
		if err != nil || lock != nil || holder == nil || holder.PID != os.Getppid() || !holder.PIDOnly {
			t.Fatalf("acquireByPID = (%v, %+v, %v)", lock, holder, err)
		}
	})

	t.Run("a dead PID is taken over, and the lock is removed on release", func(t *testing.T) {
		dir := t.TempDir()
		writeLock(t, dir, strconv.Itoa(deadPID)+"\n")
		lock, holder, err := acquireByPID(filepath.Join(dir, Name), cause)
		if err != nil || holder != nil {
			t.Fatalf("acquireByPID = (%v, %+v, %v)", lock, holder, err)
		}
		if lock.Stale != deadPID || lock.NoOSLock == nil {
			t.Errorf("Stale = %d, NoOSLock = %v", lock.Stale, lock.NoOSLock)
		}
		if err := lock.RecordPort(8123); err != nil {
			t.Fatal(err)
		}
		// Without the OS lock the marker would be a lie.
		want := Content{PID: os.Getpid(), Port: 8123}
		if got, _ := Parse([]byte(readLock(t, dir))); got != want {
			t.Errorf("contents %+v, want %+v", got, want)
		}
		lock.Release()
		if _, err := os.Stat(filepath.Join(dir, Name)); !os.IsNotExist(err) {
			t.Errorf("the lock was not removed (err=%v)", err)
		}
	})

	t.Run("a lock taken over by another agent is left to it", func(t *testing.T) {
		dir := t.TempDir()
		lock, _, err := acquireByPID(filepath.Join(dir, Name), cause)
		if err != nil {
			t.Fatal(err)
		}
		other := strconv.Itoa(deadPID) + "\n"
		writeLock(t, dir, other)
		if err := lock.RecordPort(8123); err == nil {
			t.Error("RecordPort wrote over another agent's lock")
		}
		lock.Release()
		if got := readLock(t, dir); got != other {
			t.Errorf("Release changed another agent's lock to %q", got)
		}
	})
}
