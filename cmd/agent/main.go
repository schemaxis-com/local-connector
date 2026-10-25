// Command agent is the local connector: a SQL proxy between the cloud
// platform and an Oracle database on the customer's network.
//
// The agent opens an outbound WSS connection to the platform, receives SQL
// queries, refuses any that fail the read-only check in internal/proxy, runs
// the rest on the local Oracle database, and returns the rows as JSON. It holds
// no analysis logic of its own: the analysis happens on the platform.
//
// Singleton enforcement: an OS-level lock on a file in the data directory
// (internal/lockfile) prevents running more than one agent instance per user
// account. Relaunching the binary while an instance is already running reopens
// that instance's Web UI instead of failing, or only prints its address under
// --no-browser. Closing the browser tab by mistake should never require
// killing and restarting the agent.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/schemaxis-com/local-connector/internal/branding"
	"github.com/schemaxis-com/local-connector/internal/config"
	"github.com/schemaxis-com/local-connector/internal/lockfile"
	"github.com/schemaxis-com/local-connector/internal/manager"
)

// browserOpener is a package-level indirection over openBrowser so tests can
// stub it out without spawning a real browser process.
var browserOpener = openBrowser

// readLockPort reads the UI port recorded in the lock file, if any. It retries
// briefly because a freshly started instance takes the lock (writing just its
// PID) before it has resolved and recorded its UI port. Without the retry, a
// relaunch racing that narrow window would find no address to open.
func readLockPort(lockPath string) (port int, ok bool) {
	for i := 0; i < 10; i++ {
		if data, err := os.ReadFile(lockPath); err == nil { //nolint:gosec // lockPath is always filepath.Join(DataDir(), lockfile.Name), never external input
			if c, parsed := lockfile.Parse(data); parsed && c.Port > 0 {
				return c.Port, true
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return 0, false
}

// handleExistingInstance runs when another live agent process already holds
// the lock. If that instance recorded its web UI's port, it reopens the
// browser there, covering the common "I closed the tab by mistake, let me
// just run the exe again" case, instead of treating a second launch as an
// error. With openUI false (--no-browser) it only reports the address: a
// script or service manager that starts the agent must not open a browser
// on the machine.
func handleExistingInstance(lockPath string, holder *lockfile.Holder, openUI bool) string {
	msg := "Agent already running"
	if holder.PID > 0 {
		msg = fmt.Sprintf("Agent already running (PID %d)", holder.PID)
	}
	if port, ok := readLockPort(lockPath); !ok {
		msg += ". It has not recorded its Web UI address yet"
		if openUI {
			msg += ". Run this again in a few seconds to open it"
		}
	} else if url := fmt.Sprintf("http://localhost:%d", port); openUI {
		browserOpener(url)
		msg += ". Reopened the Web UI at " + url
	} else {
		msg += ". Its Web UI is at " + url
	}
	msg += "."
	// Only a running PID says this is an agent, and it may be a program that
	// reused the PID of an agent that died. Say how to get out of it.
	if holder.PIDOnly {
		msg += fmt.Sprintf(" If no agent is running as PID %d, delete %s and start the agent again.", holder.PID, lockPath)
	}
	return msg
}

// acquireLock takes the single-instance lock in dataDir. If another instance
// is already running, it reopens that instance's Web UI (or only reports it
// under --no-browser) and exits cleanly (code 0) rather than failing. Running
// the exe a second time should never look like a crash.
func acquireLock(dataDir string, noBrowser bool) *lockfile.Lock {
	_ = os.MkdirAll(dataDir, 0700)

	lock, holder, err := lockfile.Acquire(dataDir)
	if err != nil {
		log.Fatalf("Failed to take the lock file in %s: %v", dataDir, err)
	}
	if holder != nil {
		log.Println(handleExistingInstance(filepath.Join(dataDir, lockfile.Name), holder, !noBrowser))
		os.Exit(0)
	}
	if lock.NoOSLock != nil {
		log.Printf("The file system under %s cannot lock files (%v), so a second agent is recognised by its PID alone.", dataDir, lock.NoOSLock)
	}
	if lock.Stale != 0 {
		log.Printf("Took over the stale lock left by PID %d", lock.Stale)
	}
	log.Printf("Lock acquired (PID %d): %s", os.Getpid(), lock.Path())
	return lock
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	// A .env file in the working directory may set the CLIENT_* session values
	// and AGENT_LOG_SQL.
	config.LoadDotEnv(".env")

	uiPort := flag.Int("port", 8088, "Preferred port for the local Web UI server (auto-increments if busy)")
	configPath := flag.String("config", "", fmt.Sprintf(`Path to connections.json (default: in %%APPDATA%%\%s on Windows, ~/.%s elsewhere)`, branding.AppName, branding.DirName()))
	keyPath := flag.String("key", "", fmt.Sprintf(`Path to .agent.key (default: in %%APPDATA%%\%s on Windows, ~/.%s elsewhere)`, branding.AppName, branding.DirName()))
	noBrowser := flag.Bool("no-browser", false, "Do not open a browser, at startup or when an agent is already running (its Web UI address is printed instead)")
	bindHost := flag.String("bind", manager.LoopbackHost,
		"Address the local Web UI binds to. Defaults to loopback: the UI has no "+
			"authentication and manages database credentials, so exposing it to "+
			"the network must be a deliberate choice (e.g. --bind 0.0.0.0 behind "+
			"a firewall)")
	showVersion := flag.Bool("version", false, "Print the agent version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(branding.Version)
		return
	}

	log.Println(branding.AppName + " Agent " + branding.Version + " starting...")

	// Resolve data directory early (needed for lock file). Its notes are
	// printed before the lock is taken: when an agent of the previous release
	// still runs from the old directory, acquireLock reopens that agent's UI
	// and exits, and this is the only place that says why nothing moved.
	dataDir, dataDirNotes := manager.DataDir()
	for _, note := range dataDirNotes {
		log.Println(note)
	}

	lock := acquireLock(dataDir, *noBrowser)
	defer func() {
		lock.Release()
		log.Printf("Lock released: %s", lock.Path())
	}()

	// SIGINT and SIGTERM stop the tunnels and the web UI cleanly.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		log.Printf("Received signal %v; shutting down", sig)
		cancel()
	}()

	resolvedConfig := *configPath
	if resolvedConfig == "" {
		resolvedConfig = filepath.Join(dataDir, "connections.json")
	}

	resolvedKey := *keyPath
	if resolvedKey == "" {
		resolvedKey = filepath.Join(dataDir, ".agent.key")
	}

	// The manager logs the data directory itself, to the console and the web UI.
	mgr, err := manager.NewConnectionManager(resolvedConfig, resolvedKey)
	if err != nil {
		log.Fatalf("Failed to initialize connection manager: %v", err)
	}
	// Repeated into the web UI's log, where a user of the UI reads them. The
	// console printed them before the lock was taken.
	for _, note := range dataDirNotes {
		mgr.AddUILog(note)
	}

	mgr.StartAll(ctx)
	defer mgr.StopAll()

	// Probe on the same host we will bind to: a port free on loopback is not
	// necessarily free on 0.0.0.0 (or vice versa), so probing a different
	// address than the server uses can pick a port that then fails to bind.
	port := findFreePort(*bindHost, *uiPort)
	if err := lock.RecordPort(port); err != nil {
		log.Printf("Failed to record UI port in lock file: %v", err)
	}

	// Always browse via loopback even when bound wider. The browser runs on
	// this machine, and 0.0.0.0 is not a valid destination address.
	uiURL := fmt.Sprintf("http://localhost:%d", port)
	log.Printf("Web UI will be available at: %s (bound to %s)", uiURL, *bindHost)

	if !*noBrowser {
		go browserOpener(uiURL)
	}

	if err := manager.StartServer(ctx, *bindHost, port, mgr); err != nil && ctx.Err() == nil {
		fmt.Fprintf(os.Stderr, "Agent server terminated with error: %v\n", err)
		os.Exit(1)
	}

	log.Println(branding.AppName + " Agent stopped")
}

// findFreePort finds an available TCP port on host, starting from the
// preferred one.
func findFreePort(host string, preferred int) int {
	for port := preferred; port < preferred+20; port++ {
		ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err != nil {
			continue
		}
		_ = ln.Close()
		return port
	}
	log.Printf("WARNING: Could not find a free port near %d on %s, using preferred port anyway", preferred, host)
	return preferred
}

// openBrowser opens the default system browser on the given URL.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url) //nolint:gosec // fixed binary; url is our own http://localhost:<port>, never external input
	case "darwin":
		cmd = exec.Command("open", url) //nolint:gosec // fixed binary; url is our own http://localhost:<port>, never external input
	default:
		cmd = exec.Command("xdg-open", url) //nolint:gosec // fixed binary; url is our own http://localhost:<port>, never external input
	}
	if err := cmd.Start(); err != nil {
		log.Printf("Could not open browser automatically: %v. Please navigate to %s", err, url)
	}
}
