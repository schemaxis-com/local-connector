package manager

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/schemaxis-com/local-connector/internal/branding"
	"github.com/schemaxis-com/local-connector/internal/lockfile"
)

func newTestManager(t *testing.T) *ConnectionManager {
	t.Helper()
	dir := t.TempDir()
	mgr, err := NewConnectionManager(filepath.Join(dir, "connections.json"), filepath.Join(dir, ".agent.key"))
	if err != nil {
		t.Fatalf("NewConnectionManager: %v", err)
	}
	return mgr
}

func testProfile(enabled bool) *ConnectionProfile {
	return &ConnectionProfile{
		ID: "conn_1", Label: "prod", OracleHost: "db.internal", OraclePort: "1521",
		OracleService: "ORCL", OracleUser: "reader", OraclePassword: "pw",
		TunnelURL: "wss://example.invalid/tunnel", AgentToken: "token", Enabled: enabled,
	}
}

// Unticking "enabled" in the edit form and saving must stop the tunnel that is
// running for the profile. Otherwise the database stays reachable from the
// cloud while the UI shows the connection as disabled.
func TestSavingADisabledProfileStopsItsTunnel(t *testing.T) {
	mgr := newTestManager(t)
	if err := mgr.SaveConnection(testProfile(true), ""); err != nil {
		t.Fatalf("first save: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.mu.Lock()
	mgr.runs["conn_1"] = &tunnelRun{cancel: cancel}
	mgr.mu.Unlock()

	edited := testProfile(false)
	edited.OraclePassword, edited.AgentToken = maskedSecret, maskedSecret
	if err := mgr.SaveConnection(edited, ""); err != nil {
		t.Fatalf("second save: %v", err)
	}

	if ctx.Err() == nil {
		t.Fatal("the running tunnel was not cancelled")
	}
	if got := mgr.GetConnections()[0].Status; got != StatusDisabled {
		t.Errorf("status = %q, want %q", got, StatusDisabled)
	}
}

// The masked placeholder means "keep the stored secret". A profile with no
// stored secret has nothing to keep, so the placeholder must not be saved as
// if it were ciphertext.
func TestNewProfileWithMaskedSecretIsRefused(t *testing.T) {
	mgr := newTestManager(t)
	p := testProfile(true)
	p.OraclePassword = maskedSecret
	if err := mgr.SaveConnection(p, ""); err == nil {
		t.Fatal("saved a new profile whose password is the mask placeholder")
	}
}

// A duplicate keeps the original's password while the form leaves it masked.
// The password is copied inside the agent, so the page never holds it.
func TestADuplicateKeepsTheOriginalsPassword(t *testing.T) {
	mgr := newTestManager(t)
	if err := mgr.SaveConnection(testProfile(false), ""); err != nil {
		t.Fatalf("save original: %v", err)
	}
	dup := testProfile(false)
	dup.ID, dup.Label, dup.OracleUser = "conn_2", "prod (copy)", "reader_test"
	dup.OraclePassword, dup.AgentToken = maskedSecret, "token-2"
	if err := mgr.SaveConnection(dup, "conn_1"); err != nil {
		t.Fatalf("save duplicate: %v", err)
	}

	password, err := mgr.crypto.Decrypt(mgr.profiles["conn_2"].OraclePassword)
	if err != nil || password != "pw" {
		t.Errorf("duplicate's password = %q, %v, want the original's", password, err)
	}
	token, err := mgr.crypto.Decrypt(mgr.profiles["conn_2"].AgentToken)
	if err != nil || token != "token-2" {
		t.Errorf("duplicate's token = %q, %v, want the one typed for it", token, err)
	}
}

// Each connection on the platform has its own agent token, so a duplicate
// never takes the original's, and a masked one is refused.
func TestADuplicateNeedsItsOwnToken(t *testing.T) {
	mgr := newTestManager(t)
	if err := mgr.SaveConnection(testProfile(false), ""); err != nil {
		t.Fatalf("save original: %v", err)
	}
	dup := testProfile(false)
	dup.ID, dup.OraclePassword, dup.AgentToken = "conn_2", maskedSecret, maskedSecret
	if err := mgr.SaveConnection(dup, "conn_1"); err == nil {
		t.Fatal("saved a duplicate whose agent token is the mask")
	}
	if _, ok := mgr.profiles["conn_2"]; ok {
		t.Error("the refused duplicate was stored")
	}
}

// A duplicate of a profile deleted meanwhile has no password to keep.
func TestADuplicateOfADeletedProfileNeedsThePassword(t *testing.T) {
	mgr := newTestManager(t)
	dup := testProfile(false)
	dup.ID, dup.OraclePassword = "conn_2", maskedSecret
	if err := mgr.SaveConnection(dup, "conn_1"); err == nil {
		t.Fatal("saved a duplicate of a profile that does not exist")
	}
}

// Two profiles with one token take the tunnel from each other, which the
// platform reports as another agent connecting. A token already saved for
// another profile is refused when it is typed, and an edit that leaves the
// token masked is not checked again.
func TestATokenSavedForAnotherProfileIsRefused(t *testing.T) {
	mgr := newTestManager(t)
	if err := mgr.SaveConnection(testProfile(false), ""); err != nil {
		t.Fatalf("save first: %v", err)
	}
	second := testProfile(false)
	second.ID, second.Label = "conn_2", "test"
	err := mgr.SaveConnection(second, "")
	if err == nil || !strings.Contains(err.Error(), `"prod"`) {
		t.Fatalf("SaveConnection = %v, want a refusal naming the profile that uses the token", err)
	}

	second.AgentToken = "token-2"
	if err := mgr.SaveConnection(second, ""); err != nil {
		t.Fatalf("save with its own token: %v", err)
	}
	edited := testProfile(false)
	edited.ID, edited.Label = "conn_2", "test renamed"
	edited.OraclePassword, edited.AgentToken = maskedSecret, maskedSecret
	if err := mgr.SaveConnection(edited, ""); err != nil {
		t.Errorf("an edit that keeps the token was refused: %v", err)
	}
}

// A test whose masked password cannot be resolved must not log in with the
// mask: Oracle would count it as a failed login against the account.
func TestATestWithAnUnreadablePasswordConnectsNowhere(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	var connections atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			_ = c.Close()
		}
	}()
	host, port, _ := net.SplitHostPort(ln.Addr().String())

	mgr := newTestManager(t)
	p := testProfile(false)
	p.ID, p.OracleHost, p.OraclePort, p.OraclePassword = "conn_9", host, port, maskedSecret
	if err := mgr.TestConnection(p, "conn_1"); err == nil {
		t.Fatal("TestConnection succeeded with no saved password")
	}
	if n := connections.Load(); n != 0 {
		t.Errorf("%d connections to the database, want none", n)
	}
}

// A rename keeps the saved connections only through this migration. It must
// never touch a data directory that already exists, never pull a directory
// from under an agent still running from it, and never leave the user on an
// empty directory in silence.
func TestMigrateLegacyDataDir(t *testing.T) {
	writeProfiles := func(t *testing.T, dir, content string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "connections.json"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeLock := func(t *testing.T, dir string, pid int) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, lockfile.Name), []byte(strconv.Itoa(pid)+"\n8088\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	readProfiles := func(t *testing.T, dir string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(dir, "connections.json")) //nolint:gosec // dir is a t.TempDir() path in this test, never external input
		if err != nil {
			t.Fatalf("no profiles in %s: %v", dir, err)
		}
		return string(data)
	}
	assertAbsent := func(t *testing.T, path, why string) {
		t.Helper()
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s exists (err=%v): %s", path, err, why)
		}
	}
	notesContain := func(notes []string, substr string) bool {
		for _, n := range notes {
			if strings.Contains(n, substr) {
				return true
			}
		}
		return false
	}

	t.Run("moves the most recent legacy directory and says so", func(t *testing.T) {
		root := t.TempDir()
		recent, older, current := filepath.Join(root, "Recent"), filepath.Join(root, "Older"), filepath.Join(root, "Current")
		writeProfiles(t, recent, "recent")
		writeProfiles(t, older, "older")

		got, notes := migrateLegacyDataDir([]string{recent, older}, current)

		if got != current {
			t.Fatalf("returned %s, want %s", got, current)
		}
		if p := readProfiles(t, current); p != "recent" {
			t.Errorf("carried over %q, want the most recent directory's profiles", p)
		}
		assertAbsent(t, recent, "the moved directory is still in place")
		if p := readProfiles(t, older); p != "older" {
			t.Errorf("the older legacy directory was touched: %q", p)
		}
		if !notesContain(notes, "Moved the data directory from "+recent) {
			t.Errorf("no note reports the move: %q", notes)
		}
		if !notesContain(notes, older) {
			t.Errorf("no note mentions the older directory left behind: %q", notes)
		}
	})

	t.Run("leaves an existing data directory alone", func(t *testing.T) {
		root := t.TempDir()
		old, current := filepath.Join(root, "Old"), filepath.Join(root, "Current")
		writeProfiles(t, old, "old")
		writeProfiles(t, current, "current")

		got, notes := migrateLegacyDataDir([]string{old}, current)

		if got != current || len(notes) != 0 {
			t.Fatalf("got (%s, %q), want (%s, no notes)", got, notes, current)
		}
		if p := readProfiles(t, old); p != "old" {
			t.Errorf("legacy directory touched: %q", p)
		}
		if p := readProfiles(t, current); p != "current" {
			t.Errorf("existing data directory overwritten: %q", p)
		}
	})

	t.Run("no legacy directory is a no-op", func(t *testing.T) {
		root := t.TempDir()
		current := filepath.Join(root, "Current")

		got, notes := migrateLegacyDataDir([]string{filepath.Join(root, "Missing")}, current)

		if got != current || len(notes) != 0 {
			t.Fatalf("got (%s, %q), want (%s, no notes)", got, notes, current)
		}
		assertAbsent(t, current, "the migration created the data directory with nothing to migrate")
	})

	t.Run("a legacy name equal to the current one is skipped", func(t *testing.T) {
		root := t.TempDir()
		current, old := filepath.Join(root, "Current"), filepath.Join(root, "Old")
		writeProfiles(t, old, "old")

		got, _ := migrateLegacyDataDir([]string{current, old}, current)

		if got != current || readProfiles(t, current) != "old" {
			t.Fatal("migration stopped at the current name instead of the next legacy one")
		}
	})

	t.Run("a file with a legacy name is not a data directory", func(t *testing.T) {
		root := t.TempDir()
		stray, current := filepath.Join(root, "Old"), filepath.Join(root, "Current")
		if err := os.WriteFile(stray, []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}

		got, notes := migrateLegacyDataDir([]string{stray}, current)

		if got != current || len(notes) != 0 {
			t.Fatalf("got (%s, %q), want (%s, no notes)", got, notes, current)
		}
		if _, err := os.Stat(stray); err != nil {
			t.Errorf("the stray file was moved: %v", err)
		}
	})

	// The parent of the test binary is a live process other than this one,
	// which is what the lock of a running agent of the previous release names.
	t.Run("does not move a directory an agent still runs from", func(t *testing.T) {
		root := t.TempDir()
		old, current := filepath.Join(root, "Old"), filepath.Join(root, "Current")
		writeProfiles(t, old, "old")
		writeLock(t, old, os.Getppid())

		got, notes := migrateLegacyDataDir([]string{old}, current)

		if got != old {
			t.Fatalf("returned %s, want the legacy directory %s kept in use", got, old)
		}
		if p := readProfiles(t, old); p != "old" {
			t.Errorf("the running agent's directory lost its profiles: %q", p)
		}
		assertAbsent(t, current, "the new directory was created while the old agent still runs")
		if !notesContain(notes, "PID "+strconv.Itoa(os.Getppid())) {
			t.Errorf("no note names the running agent: %q", notes)
		}
		// That lock has no OS lock behind it, so the PID may belong to
		// another program: the note says how to clear it.
		if !notesContain(notes, "delete "+filepath.Join(old, lockfile.Name)) {
			t.Errorf("no note says how to clear a lock known only by its PID: %q", notes)
		}
	})

	t.Run("does not move a directory whose lock is held", func(t *testing.T) {
		root := t.TempDir()
		old, current := filepath.Join(root, "Old"), filepath.Join(root, "Current")
		writeProfiles(t, old, "old")
		lock, holder, err := lockfile.Acquire(old)
		if err != nil || holder != nil {
			t.Fatalf("Acquire = (%v, %+v, %v)", lock, holder, err)
		}
		t.Cleanup(lock.Release)

		got, notes := migrateLegacyDataDir([]string{old}, current)

		if got != old {
			t.Fatalf("returned %s, want the legacy directory %s kept in use", got, old)
		}
		assertAbsent(t, current, "the new directory was created while the old agent still runs")
		if !notesContain(notes, "PID "+strconv.Itoa(os.Getpid())) || notesContain(notes, "delete") {
			t.Errorf("the note does not name the holder, or doubts it: %q", notes)
		}
	})

	// An agent died and the OS gave its PID to a program that is running
	// (the parent of the test binary stands in for it). The lock is not held,
	// so it does not defer the move.
	t.Run("a lock whose PID was reused does not block the move", func(t *testing.T) {
		root := t.TempDir()
		old, current := filepath.Join(root, "Old"), filepath.Join(root, "Current")
		writeProfiles(t, old, "old")
		if err := os.WriteFile(filepath.Join(old, lockfile.Name), []byte(strconv.Itoa(os.Getppid())+"\nport=8088\nlock=os\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		got, _ := migrateLegacyDataDir([]string{old}, current)

		if got != current || readProfiles(t, current) != "old" {
			t.Fatalf("a reused PID stopped the migration (got %s)", got)
		}
	})

	t.Run("a stale lock does not block the move", func(t *testing.T) {
		root := t.TempDir()
		old, current := filepath.Join(root, "Old"), filepath.Join(root, "Current")
		writeProfiles(t, old, "old")
		writeLock(t, old, 999999)

		got, _ := migrateLegacyDataDir([]string{old}, current)

		if got != current || readProfiles(t, current) != "old" {
			t.Fatalf("a dead agent's lock stopped the migration (got %s)", got)
		}
	})

	t.Run("a failed rename keeps the legacy directory in use and says so", func(t *testing.T) {
		root := t.TempDir()
		old, current := filepath.Join(root, "Old"), filepath.Join(root, "Current")
		writeProfiles(t, old, "old")
		saved := renameDir
		t.Cleanup(func() { renameDir = saved })
		renameDir = func(string, string) error { return errors.New("access is denied") }

		got, notes := migrateLegacyDataDir([]string{old}, current)

		if got != old {
			t.Fatalf("returned %s, want the legacy directory %s used in place", got, old)
		}
		assertAbsent(t, current, "an empty data directory would stop every later start from retrying the move")
		if !notesContain(notes, "access is denied") {
			t.Errorf("the rename error is not reported: %q", notes)
		}
	})

	t.Run("a rename lost to a concurrent start uses the moved directory", func(t *testing.T) {
		root := t.TempDir()
		old, current := filepath.Join(root, "Old"), filepath.Join(root, "Current")
		writeProfiles(t, old, "old")
		saved := renameDir
		t.Cleanup(func() { renameDir = saved })
		renameDir = func(from, to string) error {
			if err := os.Rename(from, to); err != nil {
				t.Fatal(err)
			}
			return errors.New("the other instance moved it first")
		}

		got, notes := migrateLegacyDataDir([]string{old}, current)

		if got != current || readProfiles(t, current) != "old" {
			t.Fatalf("returned %s, want the directory the other start moved into place", got)
		}
		if notesContain(notes, "Could not move") {
			t.Errorf("a move that happened is reported as failed: %q", notes)
		}
	})

	t.Run("a legacy directory gone before the rename is not recreated", func(t *testing.T) {
		root := t.TempDir()
		old, current := filepath.Join(root, "Old"), filepath.Join(root, "Current")
		writeProfiles(t, old, "old")
		saved := renameDir
		t.Cleanup(func() { renameDir = saved })
		renameDir = func(from, _ string) error {
			if err := os.RemoveAll(from); err != nil {
				t.Fatal(err)
			}
			return errors.New("the system cannot find the file specified")
		}

		got, notes := migrateLegacyDataDir([]string{old}, current)

		if got != current {
			t.Fatalf("returned %s, want %s: the vanished legacy directory would be recreated empty", got, current)
		}
		if notesContain(notes, "saved connections are all there") {
			t.Errorf("a note claims connections that no longer exist: %q", notes)
		}
	})
}

// dataDirRoot points DataDir at a temporary root on the platform's own path
// layout: the product name as is under %APPDATA% on Windows, the lower-case dot
// directory in the home directory elsewhere. It returns the path a name maps to.
func dataDirRoot(t *testing.T) func(name string) string {
	t.Helper()
	root := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("APPDATA", root)
		return func(name string) string { return filepath.Join(root, name) }
	}
	t.Setenv("HOME", root)
	return func(name string) string { return filepath.Join(root, "."+strings.ToLower(name)) }
}

// setBranding swaps the product name (left as is when empty) and the legacy
// names for one test.
func setBranding(t *testing.T, appName string, legacy []string) {
	t.Helper()
	savedName, savedLegacy := branding.AppName, branding.LegacyAppNames
	t.Cleanup(func() { branding.AppName, branding.LegacyAppNames = savedName, savedLegacy })
	if appName != "" {
		branding.AppName = appName
	}
	branding.LegacyAppNames = legacy
}

func seedDataDir(t *testing.T, dir, profiles string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "connections.json"), []byte(profiles), 0o600); err != nil {
		t.Fatal(err)
	}
}

// DataDir wires LegacyAppNames to the migration, most recent name first: the
// list is appended to on each rename, so its last entry holds the latest data.
func TestDataDirMigratesTheMostRecentListedName(t *testing.T) {
	setBranding(t, "", []string{"OlderName", "OldName"})
	path := dataDirRoot(t)
	seedDataDir(t, path("OlderName"), "older")
	seedDataDir(t, path("OldName"), "old")

	dir, notes := DataDir()

	if dir != path(branding.AppName) {
		t.Fatalf("DataDir() = %s, want %s", dir, path(branding.AppName))
	}
	data, err := os.ReadFile(filepath.Join(dir, "connections.json")) //nolint:gosec // dir is under t.TempDir() in this test, never external input
	if err != nil || string(data) != "old" {
		t.Fatalf("DataDir() carried over %q (err=%v), want the most recent name's profiles", data, err)
	}
	if len(notes) == 0 {
		t.Error("DataDir() moved a directory without a note saying so")
	}
}

// When the move has to wait, DataDir must hand back the legacy directory and
// create nothing at the new path: an empty directory there would make every
// later start skip the migration and leave the saved connections behind.
func TestDataDirKeepsTheLegacyDirectoryWhileItsAgentRuns(t *testing.T) {
	setBranding(t, "", []string{"OldName"})
	path := dataDirRoot(t)
	legacy := path("OldName")
	seedDataDir(t, legacy, "old")
	if err := os.WriteFile(filepath.Join(legacy, lockfile.Name), []byte(strconv.Itoa(os.Getppid())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	dir, _ := DataDir()

	if dir != legacy {
		t.Fatalf("DataDir() = %s, want the legacy directory %s", dir, legacy)
	}
	if _, err := os.Lstat(path(branding.AppName)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("DataDir() created the new data directory while the old agent still runs (err=%v)", err)
	}
}

// A build that sets AppName with -X is not the product the legacy names
// belong to, and must not move a normal installation's data into itself.
func TestDataDirDoesNotMigrateInABuildWithAnOverriddenName(t *testing.T) {
	setBranding(t, "InternalBuild", []string{"OldName"})
	path := dataDirRoot(t)
	seedDataDir(t, path("OldName"), "old")

	dir, notes := DataDir()

	if dir != path("InternalBuild") {
		t.Fatalf("DataDir() = %s, want %s", dir, path("InternalBuild"))
	}
	if _, err := os.Stat(filepath.Join(path("OldName"), "connections.json")); err != nil {
		t.Fatalf("the overridden build moved the normal installation's data: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "connections.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the overridden build's directory holds migrated profiles (err=%v)", err)
	}
	if len(notes) != 0 {
		t.Errorf("unexpected notes: %q", notes)
	}
}
