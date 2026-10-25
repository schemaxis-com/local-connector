// Package manager keeps the agent's connection profiles, runs a tunnel for
// each enabled one, and serves the local web UI that edits them.
package manager

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/schemaxis-com/local-connector/internal/branding"
	"github.com/schemaxis-com/local-connector/internal/config"
	"github.com/schemaxis-com/local-connector/internal/lockfile"
	"github.com/schemaxis-com/local-connector/internal/proxy"
	"github.com/schemaxis-com/local-connector/internal/tunnel"
)

const (
	StatusConnected    = "connected"
	StatusReconnecting = "reconnecting"
	StatusDisconnected = "disconnected"
	StatusError        = "error"
	StatusDisabled     = "disabled"
)

// maskedSecret stands in for a stored password or token in every profile the
// UI reads. When the UI sends it back unchanged, the stored ciphertext is kept.
const maskedSecret = "••••••••" //nolint:gosec // a display mask, not a credential

// ErrProfileNotFound is returned for a profile ID the manager does not hold.
var ErrProfileNotFound = errors.New("connection profile not found")

// ConnectionProfile represents a stored DB and tunnel configuration.
// Sensitive fields (OraclePassword, AgentToken) are stored encrypted (base64) at rest.
type ConnectionProfile struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// A service name or a SID identifies the database, as in SQL Developer's
	// "Service Name vs SID" choice. The web UI saves one of them; a profile
	// that holds both connects by the SID.
	OracleHost     string `json:"oracle_host"`
	OraclePort     string `json:"oracle_port"`
	OracleService  string `json:"oracle_service"`
	OracleSID      string `json:"oracle_sid"`
	OracleUser     string `json:"oracle_user"`
	OraclePassword string `json:"oracle_password"` // Ciphertext
	TunnelURL      string `json:"tunnel_url"`
	AgentToken     string `json:"agent_token"` // Ciphertext
	Enabled        bool   `json:"enabled"`
}

// ConnectionStatus represents the live status of a connection.
type ConnectionStatus struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Status    string `json:"status"` // one of the Status* constants
	LastError string `json:"last_error,omitempty"`
	Enabled   bool   `json:"enabled"`
}

// LogEntry is a time-stamped log message for the UI.
type LogEntry struct {
	Timestamp string `json:"timestamp"`
	Message   string `json:"message"`
}

// ConnectionManager manages multiple active database connections and tunnels.
type ConnectionManager struct {
	mu           sync.RWMutex // Protects profiles, statuses, runs
	logMu        sync.Mutex   // Separate mutex for logs, avoids deadlock
	profilesPath string
	crypto       *CryptoHelper
	profiles     map[string]*ConnectionProfile
	statuses     map[string]*ConnectionStatus
	runs         map[string]*tunnelRun
	running      sync.WaitGroup // tunnel goroutines, stopped or not
	logs         []LogEntry
	maxLogs      int
}

// tunnelState is how far a running connection's tunnel has got.
type tunnelState int

const (
	tunnelOpening tunnelState = iota
	tunnelUp
	tunnelDown
)

// tunnelRun is one start of a connection's tunnel, from startTunnel until it
// is stopped or replaced. Only the profile's current run may change its
// status: a run that was stopped can still be finishing an Oracle connect or
// a tunnel read, and what it would report is no longer true.
type tunnelRun struct {
	cancel context.CancelFunc

	// What the run knows of its two links, guarded by ConnectionManager.mu.
	tunnel    tunnelState
	tunnelErr string // why the tunnel is down
	dbErr     string // why the database does not answer, empty while it does
}

// linkStatus is what the UI shows for a run that has opened its database:
// connected only while both the tunnel and the database are.
func (r *tunnelRun) linkStatus() (status, detail string) {
	var parts []string
	if r.dbErr != "" {
		parts = append(parts, "The database is not answering: "+r.dbErr)
	}
	switch r.tunnel {
	case tunnelUp:
		if r.dbErr == "" {
			return StatusConnected, ""
		}
		parts = append(parts, "The tunnel is up, so queries from the platform fail until the database is back")
	case tunnelOpening:
		if r.dbErr == "" {
			parts = append(parts, "Database connected")
		}
		parts = append(parts, "Establishing tunnel...")
	case tunnelDown:
		parts = append(parts, r.tunnelErr)
	}
	return StatusReconnecting, strings.Join(parts, ". ")
}

// database is what a running connection uses of its Oracle connection.
type database interface {
	tunnel.QueryExecutor
	Ping(ctx context.Context) error
	LastAnswer() time.Time
	Close() error
}

// Indirections over opening the database and the timing around it, so tests
// can run a connection's whole life without Oracle.
var (
	openDatabase = func(ctx context.Context, cfg *config.Config) (database, error) {
		e, err := proxy.NewExecutor(ctx, cfg)
		if err != nil {
			return nil, err
		}
		return e, nil
	}
	// databaseRetryDelay is the wait between attempts to open the database.
	databaseRetryDelay = 5 * time.Second
	// databaseCheckInterval is how often a running connection checks that
	// its database still answers.
	databaseCheckInterval = 10 * time.Second
)

// databaseCheckTimeout bounds one check. It is longer than a query's own
// timeout, so a check that waits for a session behind slow queries sees one
// of them finish before it gives up.
const databaseCheckTimeout = proxy.QueryTimeout + 5*time.Second

// DataDir returns a stable, platform-specific directory for storing agent data:
// %APPDATA%\<AppName> on Windows, ~/.<appname> elsewhere, and notes on what it
// did to reach it, for the console and the web UI's log.
//
// On first run after a rebrand it moves the data directory of a previous name
// (branding.LegacyAppNames) to the new location. When that move cannot happen
// yet, the directory returned is the previous one, used where it is, so the
// saved connections stay in use and the move is tried again at the next start.
// A build that overrides AppName with -X never migrates.
func DataDir() (dir string, notes []string) {
	var base string
	var legacyNames []string
	if runtime.GOOS == "windows" {
		base = os.Getenv("APPDATA")
		if base == "" {
			base = filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Roaming")
		}
		dir = filepath.Join(base, branding.AppName)
		legacyNames = branding.LegacyAppNames
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		base = home
		dir = filepath.Join(home, "."+branding.DirName())
		for _, name := range branding.LegacyDirNames() {
			legacyNames = append(legacyNames, "."+name)
		}
	}

	// The list is kept oldest first; the most recent name holds the most
	// recent data, so it is looked for first.
	var legacyDirs []string
	if !branding.AppNameOverridden() {
		for i := len(legacyNames) - 1; i >= 0; i-- {
			legacyDirs = append(legacyDirs, filepath.Join(base, legacyNames[i]))
		}
	}

	dir, notes = migrateLegacyDataDir(legacyDirs, dir)
	_ = os.MkdirAll(dir, 0700)
	return dir, notes
}

// Indirections over the lock check and the rename, so tests can reproduce a
// running agent of the previous release and a rename the OS refuses.
var (
	liveLockHolder = lockfile.LiveHolder
	renameDir      = os.Rename
)

// migrateLegacyDataDir moves the first existing directory in legacyDirs
// (ordered most recent name first) to dir, preserving connections.json and
// .agent.key for upgrading users, and returns the directory to use now with
// notes on what happened.
//
// It never touches dir when anything is already there, and never moves a
// directory an agent is still running from: that agent would lose its data
// directory from under it (every later save of a profile fails) and its lock
// would travel with the files, so the new agent would read it as a second
// instance of itself. In that case, and when the rename fails, the legacy
// directory is returned so this run keeps the saved connections and the
// singleton lock sees the agent still running there; dir is not created, so
// the next start tries again.
func migrateLegacyDataDir(legacyDirs []string, dir string) (string, []string) {
	if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
		return dir, nil
	}

	var found []string
	for _, legacyDir := range legacyDirs {
		if legacyDir == dir {
			continue
		}
		if info, err := os.Stat(legacyDir); err == nil && info.IsDir() {
			found = append(found, legacyDir)
		}
	}
	if len(found) == 0 {
		return dir, nil
	}

	source := found[0]
	var notes []string
	for _, older := range found[1:] {
		notes = append(notes, fmt.Sprintf(
			"Left the data directory %s where it is. It belongs to an older name than %s, which holds the more recent data.",
			older, source))
	}

	if holder := liveLockHolder(source); holder != nil {
		user := "an agent that is still running uses it"
		if holder.PID > 0 {
			user = fmt.Sprintf("the agent running as PID %d still uses it", holder.PID)
		}
		note := fmt.Sprintf(
			"Did not move the data directory %s to %s: %s. "+
				"Close that agent and start this one again to finish the move. Your saved connections stay where they are until then.",
			source, dir, user)
		// Only a running PID says it is an agent, and it may be a program that
		// reused the PID of an agent that died.
		if holder.PIDOnly {
			note += fmt.Sprintf(" If no agent is running as PID %d, delete %s and start this one again.",
				holder.PID, filepath.Join(source, lockfile.Name))
		}
		notes = append(notes, note)
		return source, notes
	}

	if err := renameDir(source, dir); err != nil {
		// Another agent started at the same moment may have moved it first,
		// or the directory went away: either way there is nothing left to use
		// in place, and recreating it empty would only strand the next start.
		if _, statErr := os.Lstat(dir); statErr == nil {
			return dir, notes
		}
		if _, statErr := os.Lstat(source); errors.Is(statErr, fs.ErrNotExist) {
			return dir, notes
		}
		// The paths are in the note already; keep only the OS's reason, whose
		// own full stop (Windows ends its messages with one) is dropped.
		reason := err
		var linkErr *os.LinkError
		if errors.As(err, &linkErr) {
			reason = linkErr.Err
		}
		notes = append(notes, fmt.Sprintf(
			"Could not move the data directory %s to %s: %s. "+
				"This run keeps using %s, so your saved connections are all there, and the move is tried again at the next start.",
			source, dir, strings.TrimSuffix(reason.Error(), "."), source))
		return source, notes
	}

	notes = append(notes, fmt.Sprintf(
		"Moved the data directory from %s to %s after the product rename. Your saved connections came with it.",
		source, dir))
	return dir, notes
}

// NewConnectionManager creates a new manager.
func NewConnectionManager(profilesPath, keyPath string) (*ConnectionManager, error) {
	crypto, err := NewCryptoHelper(keyPath)
	if err != nil {
		return nil, fmt.Errorf("init crypto: %w", err)
	}
	if profilesPath == "" {
		profilesPath = "connections.json"
	}
	mgr := &ConnectionManager{
		profilesPath: profilesPath,
		crypto:       crypto,
		profiles:     make(map[string]*ConnectionProfile),
		statuses:     make(map[string]*ConnectionStatus),
		runs:         make(map[string]*tunnelRun),
		logs:         make([]LogEntry, 0),
		maxLogs:      200,
	}
	mgr.AddLog("Connection Manager initialized")
	mgr.AddLog(fmt.Sprintf("Data directory: %s", filepath.Dir(profilesPath)))
	if err := mgr.loadProfiles(); err != nil {
		mgr.AddLog(fmt.Sprintf("Failed to load profiles: %v", err))
	}
	return mgr, nil
}

// AddLog adds a message to the web UI's log and writes it to the console.
func (m *ConnectionManager) AddLog(msg string) {
	m.AddUILog(msg)
	log.Printf("[MGR] %s", msg)
}

// AddUILog adds a message to the web UI's log only, for one the console has
// already shown.
func (m *ConnectionManager) AddUILog(msg string) {
	m.logMu.Lock()
	defer m.logMu.Unlock()
	entry := LogEntry{
		Timestamp: time.Now().Format("2006-01-02 15:04:05"),
		Message:   msg,
	}
	m.logs = append(m.logs, entry)
	if len(m.logs) > m.maxLogs {
		m.logs = m.logs[len(m.logs)-m.maxLogs:]
	}
}

// GetLogs returns all stored log entries.
func (m *ConnectionManager) GetLogs() []LogEntry {
	m.logMu.Lock()
	defer m.logMu.Unlock()
	logsCopy := make([]LogEntry, len(m.logs))
	copy(logsCopy, m.logs)
	return logsCopy
}

func (m *ConnectionManager) loadProfiles() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, err := os.ReadFile(m.profilesPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var list []*ConnectionProfile
	if err := json.Unmarshal(data, &list); err != nil {
		return err
	}
	for _, p := range list {
		m.profiles[p.ID] = p
		m.statuses[p.ID] = &ConnectionStatus{
			ID: p.ID, Label: p.Label, Status: StatusDisabled, Enabled: p.Enabled,
		}
		if p.Enabled {
			m.statuses[p.ID].Status = StatusDisconnected
		}
	}
	return nil
}

// byLabel orders two entries by label, and by ID when the labels are equal,
// so the UI, the console and connections.json list profiles in one order.
func byLabel(labelA, idA, labelB, idB string) int {
	return cmp.Or(cmp.Compare(labelA, labelB), cmp.Compare(idA, idB))
}

func sortProfiles(list []*ConnectionProfile) {
	slices.SortFunc(list, func(a, b *ConnectionProfile) int { return byLabel(a.Label, a.ID, b.Label, b.ID) })
}

func (m *ConnectionManager) saveProfiles() error {
	list := slices.Collect(maps.Values(m.profiles))
	sortProfiles(list)
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.profilesPath, data, 0600)
}

// StartAll starts tunnels for all enabled connection profiles.
func (m *ConnectionManager) StartAll(ctx context.Context) {
	m.mu.Lock()
	profilesCopy := slices.Collect(maps.Values(m.profiles))
	m.mu.Unlock()
	sortProfiles(profilesCopy)
	for _, p := range profilesCopy {
		if p.Enabled {
			m.AddLog(fmt.Sprintf("Starting tunnel for: %s", p.Label))
			m.startTunnel(ctx, p.ID)
		}
	}
}

// StopAll stops all running tunnels.
func (m *ConnectionManager) StopAll() {
	m.AddLog("Stopping all connections...")
	m.mu.Lock()
	defer m.mu.Unlock()
	for id := range m.runs {
		m.stopRunLocked(id)
	}
}

// stopRunLocked stops the profile's current run, if any. The caller holds
// m.mu and sets the status that follows.
func (m *ConnectionManager) stopRunLocked(id string) {
	if run, ok := m.runs[id]; ok {
		run.cancel()
		delete(m.runs, id)
	}
}

// startTunnel starts a proxy and WebSocket client in a separate goroutine,
// replacing the profile's current run, if any.
func (m *ConnectionManager) startTunnel(parentCtx context.Context, id string) {
	m.mu.Lock()
	p, ok := m.profiles[id]
	if !ok {
		m.mu.Unlock()
		return
	}
	m.stopRunLocked(id)
	connCtx, connCancel := context.WithCancel(parentCtx)
	run := &tunnelRun{cancel: connCancel}
	m.runs[id] = run
	if s, ok := m.statuses[id]; ok {
		s.Status, s.LastError = StatusDisconnected, ""
	}

	// A copy taken under the lock: a later save replaces the profile. The URL
	// is trimmed as the web UI trims it on save, for a connections.json edited
	// by hand.
	profile := *p
	label, tunnelURL := profile.Label, strings.TrimSpace(profile.TunnelURL)
	m.mu.Unlock()

	m.running.Add(1)
	go func() {
		defer m.running.Done()
		defer connCancel()

		decryptedPass, err1 := m.crypto.Decrypt(profile.OraclePassword)
		decryptedToken, err2 := m.crypto.Decrypt(profile.AgentToken)
		if err := errors.Join(err1, err2); err != nil {
			m.AddLog(fmt.Sprintf("[%s] Could not decrypt the stored credentials: %v", label, err))
			m.setStatus(run, id, StatusError, "Could not decrypt the stored credentials")
			return
		}

		// Checked before the database connection is opened: the web UI
		// refuses such a URL when a profile is saved, but a connections.json
		// edited by hand can still carry one.
		if err := tunnel.ValidateURL(tunnelURL); err != nil {
			m.AddLog(fmt.Sprintf("[%s] Tunnel not started: %v", label, err))
			m.setStatus(run, id, StatusError, err.Error())
			return
		}

		cfg := &config.Config{
			OracleHost:     profile.OracleHost,
			OraclePort:     profile.OraclePort,
			OracleService:  profile.OracleService,
			OracleSID:      profile.OracleSID,
			OracleUser:     profile.OracleUser,
			OraclePassword: decryptedPass,
			TunnelURL:      tunnelURL,
			AgentToken:     decryptedToken,
		}

		db, refused := m.connectDatabase(connCtx, run, id, label, cfg)
		if refused != nil {
			m.stopForRefusal(run, id, label, refused)
			return
		}
		if db == nil {
			m.AddLog(fmt.Sprintf("[%s] Tunnel stopped", label))
			return
		}
		m.AddLog(fmt.Sprintf("[%s] Database connected successfully", label))
		m.setLinks(run, id, func(r *tunnelRun) { r.tunnel = tunnelOpening })

		client := tunnel.NewClient(cfg, db).
			WithLogger(func(msg string) {
				m.AddLog(fmt.Sprintf("[%s] %s", label, msg))
			}).
			WithOnConnect(func() {
				m.setLinks(run, id, func(r *tunnelRun) { r.tunnel = tunnelUp })
			}).
			WithOnDisconnect(func(err error, attempt int) {
				// The tunnel client logs its own backoff delay; repeating a
				// delay here would name the wrong one.
				detail := fmt.Sprintf("Tunnel not connected (attempt %d): %s", attempt, strings.TrimSuffix(err.Error(), "."))
				m.setLinks(run, id, func(r *tunnelRun) { r.tunnel, r.tunnelErr = tunnelDown, detail })
			})

		// A login the database refuses while the tunnel runs (the password
		// was changed, and the pool needs a new session) ends the run too, and
		// so does a network encryption it refuses.
		checksCtx, stopChecks := context.WithCancel(connCtx)
		checksDone := make(chan struct{})
		go func() {
			defer close(checksDone)
			if refused = m.watchDatabase(checksCtx, run, id, label, db, client.ReportDatabase); refused != nil {
				connCancel()
			}
		}()

		err := client.Run(connCtx)
		stopChecks()
		<-checksDone
		_ = db.Close()

		switch {
		case refused != nil:
			m.stopForRefusal(run, id, label, refused)
		case connCtx.Err() != nil:
			// Whoever stopped the run has set the status that follows.
			m.AddLog(fmt.Sprintf("[%s] Tunnel stopped", label))
		case errors.Is(err, tunnel.ErrDuplicateRejection):
			// The client has logged the takeover.
			m.setStatus(run, id, StatusError, "Another agent connected with this connection's token and took the tunnel over. "+
				"Turn this connection off and on again to take it back, which disconnects the other agent.")
		default:
			// Run ends by itself only for a takeover or a tunnel URL it
			// refuses, and it has logged either.
			m.setStatus(run, id, StatusError, fmt.Sprintf("Tunnel stopped: %v", err))
		}
	}()
}

// connectDatabase opens the connection's database, retrying until it answers.
// It returns a nil database once ctx is cancelled, and stops with the error
// when the database refuses the login or the network encryption, which
// retrying would only repeat.
func (m *ConnectionManager) connectDatabase(ctx context.Context, run *tunnelRun, id, label string, cfg *config.Config) (database, error) {
	m.AddLog(fmt.Sprintf("[%s] Connecting to database: %s as %s...", label, cfg.Target(), cfg.OracleUser))
	for attempt := 1; ; attempt++ {
		db, err := openDatabase(ctx, cfg)
		if ctx.Err() != nil {
			// Stopped while connecting: the outcome of this attempt is no
			// longer news, and a connection it opened is not used.
			if err == nil {
				_ = db.Close()
			}
			return nil, nil
		}
		if err == nil {
			return db, nil
		}
		if refusedByTheDatabase(err) {
			return nil, err
		}
		detail := fmt.Sprintf("Oracle connect failed (attempt %d): %v", attempt, err)
		m.AddLog(fmt.Sprintf("[%s] %s; retrying in %ds", label, detail, int(databaseRetryDelay/time.Second)))
		m.setStatus(run, id, StatusReconnecting, detail)
		timer := time.NewTimer(databaseRetryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, nil
		case <-timer.C:
		}
	}
}

// refusedByTheDatabase reports whether err is a refusal the database repeats
// on every attempt until someone changes the account or its network settings:
// a refused login, or a network encryption it will not agree on.
func refusedByTheDatabase(err error) bool {
	return proxy.IsLoginRefused(err) || proxy.IsEncryptionRefused(err)
}

// stopForRefusal reports a login or a network encryption the database
// refused, which ends the run. Every further attempt would be refused the same
// way: a refused network encryption adds lines to the database's alert log
// each time, and a refused login counts against the account, which Oracle
// locks after a few (FAILED_LOGIN_ATTEMPTS, ten by default).
func (m *ConnectionManager) stopForRefusal(run *tunnelRun, id, label string, err error) {
	if proxy.IsEncryptionRefused(err) {
		m.AddLog(fmt.Sprintf("[%s] The database refused the connection while setting up network encryption: %v. "+
			"Stopped retrying, because the database would refuse and log every attempt", label, err))
		m.setStatus(run, id, StatusError, fmt.Sprintf("The database refused the connection while setting up network encryption: %v. "+
			"The agent stopped retrying, because the database would refuse and log every attempt. "+
			"Turn the connection off and on once the database's network settings change.", err))
		return
	}
	m.AddLog(fmt.Sprintf("[%s] The database refused the login: %v. Stopped retrying so that the account is not locked", label, err))
	m.setStatus(run, id, StatusError, fmt.Sprintf("The database refused the login: %v. "+
		"The agent stopped retrying so that the account is not locked. "+
		"Correct the credentials and save the connection, or turn it off and on once the account is fixed.", err))
}

// watchDatabase checks that the database still answers for as long as the
// tunnel runs. It logs when the database stops answering and when it answers
// again, not every failed check: the reason often names a new local port each
// time, and the card shows the latest one.
//
// The tunnel stays up while the database is down. The platform then gets the
// database's own error for each query rather than an agent gone offline, and
// the session pool reconnects by itself once the database is back. A refused
// login or network encryption is the exception: watchDatabase returns it, and
// the run stops. Each change is also passed to report, which tells the
// platform.
func (m *ConnectionManager) watchDatabase(ctx context.Context, run *tunnelRun, id, label string, db database, report func(answering bool)) error {
	ticker := time.NewTicker(databaseCheckInterval)
	defer ticker.Stop()
	since := time.Now()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		problem := ""
		err := checkDatabase(ctx, db, since)
		since = time.Now()
		if ctx.Err() != nil {
			return nil
		}
		if refusedByTheDatabase(err) {
			return err
		}
		if err != nil {
			problem = strings.TrimSuffix(err.Error(), ".")
		}
		var before string
		if !m.setLinks(run, id, func(r *tunnelRun) { before, r.dbErr = r.dbErr, problem }) {
			return nil
		}
		switch {
		case problem == "" && before != "":
			m.AddLog(fmt.Sprintf("[%s] The database is answering again", label))
			report(true)
		case problem != "" && before == "":
			m.AddLog(fmt.Sprintf("[%s] The database is not answering: %s", label, problem))
			report(false)
		}
	}
}

// checkDatabase reports whether the database answers. A statement that
// completed after since proves it without a round trip, which also keeps the
// check from queueing for a session behind queries that hold the whole pool.
// For the same reason a ping that fails while a statement completes is not
// believed: it was kept waiting, not refused.
func checkDatabase(ctx context.Context, db database, since time.Time) error {
	if db.LastAnswer().After(since) {
		return nil
	}
	started := time.Now()
	pingCtx, cancel := context.WithTimeout(ctx, databaseCheckTimeout)
	defer cancel()
	err := db.Ping(pingCtx)
	switch {
	case err == nil, !db.LastAnswer().Before(started):
		return nil
	case errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil:
		return fmt.Errorf("no answer within %s", databaseCheckTimeout)
	default:
		return err
	}
}

// setStatus shows status and detail for the profile, if run is still its
// current run.
func (m *ConnectionManager) setStatus(run *tunnelRun, id, status, detail string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.runs[id] != run {
		return
	}
	if s, ok := m.statuses[id]; ok {
		s.Status, s.LastError = status, detail
	}
}

// setLinks applies update to what run knows of its links and shows the
// status that follows, if run is still the profile's current run. It reports
// whether it was.
func (m *ConnectionManager) setLinks(run *tunnelRun, id string, update func(*tunnelRun)) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.runs[id] != run {
		return false
	}
	update(run)
	if s, ok := m.statuses[id]; ok {
		s.Status, s.LastError = run.linkStatus()
	}
	return true
}

// GetConnections returns a list of profiles with their current status.
func (m *ConnectionManager) GetConnections() []*ConnectionStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	// Never nil, so that no connections is sent as [] rather than null.
	result := make([]*ConnectionStatus, 0, len(m.statuses))
	for _, s := range m.statuses {
		result = append(result, &ConnectionStatus{
			ID: s.ID, Label: s.Label, Status: s.Status,
			LastError: s.LastError, Enabled: s.Enabled,
		})
	}
	slices.SortFunc(result, func(a, b *ConnectionStatus) int { return byLabel(a.Label, a.ID, b.Label, b.ID) })
	return result
}

// GetProfiles returns a masked list of profiles (safe for JSON responses).
func (m *ConnectionManager) GetProfiles() []*ConnectionProfile {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]*ConnectionProfile, 0, len(m.profiles))
	for _, p := range m.profiles {
		result = append(result, &ConnectionProfile{
			ID: p.ID, Label: p.Label,
			OracleHost: p.OracleHost, OraclePort: p.OraclePort,
			OracleService: p.OracleService, OracleSID: p.OracleSID,
			OracleUser:     p.OracleUser,
			OraclePassword: maskedSecret,
			TunnelURL:      p.TunnelURL,
			AgentToken:     maskedSecret,
			Enabled:        p.Enabled,
		})
	}
	sortProfiles(result)
	return result
}

// TestConnection pings the target Oracle database synchronously.
func (m *ConnectionManager) TestConnection(p *ConnectionProfile) error {
	m.AddLog(fmt.Sprintf("Testing connection to: %s...", p.Label))
	password := p.OraclePassword
	if p.ID != "" && password == maskedSecret {
		m.mu.RLock()
		existing, ok := m.profiles[p.ID]
		if ok {
			decrypted, err := m.crypto.Decrypt(existing.OraclePassword)
			if err == nil {
				password = decrypted
			}
		}
		m.mu.RUnlock()
	}
	cfg := &config.Config{
		OracleHost: p.OracleHost, OraclePort: p.OraclePort,
		OracleService: p.OracleService, OracleSID: p.OracleSID,
		OracleUser:     p.OracleUser,
		OraclePassword: password,
	}
	executor, err := proxy.NewExecutor(context.Background(), cfg)
	if err != nil {
		m.AddLog(fmt.Sprintf("Test connection failed: %v", err))
		return err
	}
	defer func() { _ = executor.Close() }()
	m.AddLog("Test connection succeeded!")
	return nil
}

// SaveConnection adds or updates a connection profile.
func (m *ConnectionManager) SaveConnection(p *ConnectionProfile) error {
	m.AddLog(fmt.Sprintf("Saving connection profile: %s", p.Label))
	m.mu.Lock()
	defer m.mu.Unlock()

	existing, isUpdate := m.profiles[p.ID]
	if !isUpdate && (p.OraclePassword == maskedSecret || p.AgentToken == maskedSecret) {
		return fmt.Errorf("a new connection needs the password and the agent token")
	}

	if p.OraclePassword == maskedSecret {
		p.OraclePassword = existing.OraclePassword
	} else {
		encPass, err := m.crypto.Encrypt(p.OraclePassword)
		if err != nil {
			return fmt.Errorf("encrypt password: %w", err)
		}
		p.OraclePassword = encPass
	}

	if p.AgentToken == maskedSecret {
		p.AgentToken = existing.AgentToken
	} else {
		encToken, err := m.crypto.Encrypt(p.AgentToken)
		if err != nil {
			return fmt.Errorf("encrypt token: %w", err)
		}
		p.AgentToken = encToken
	}

	oldStatus, hadStatus := m.statuses[p.ID]
	var oldStatusCopy *ConnectionStatus
	if hadStatus {
		copied := *oldStatus
		oldStatusCopy = &copied
	}

	m.profiles[p.ID] = p
	if _, ok := m.statuses[p.ID]; !ok {
		m.statuses[p.ID] = &ConnectionStatus{ID: p.ID, Label: p.Label, Status: StatusDisabled, Enabled: p.Enabled}
	} else {
		m.statuses[p.ID].Label = p.Label
		m.statuses[p.ID].Enabled = p.Enabled
	}

	if err := m.saveProfiles(); err != nil {
		if isUpdate {
			m.profiles[p.ID] = existing
		} else {
			delete(m.profiles, p.ID)
		}
		if hadStatus {
			m.statuses[p.ID] = oldStatusCopy
		} else {
			delete(m.statuses, p.ID)
		}
		return fmt.Errorf("save profiles: %w", err)
	}

	// Saving a profile with "enabled" unticked must stop a tunnel that is
	// already running for it, or the database stays reachable while the UI
	// shows the connection as disabled.
	if !p.Enabled {
		m.statuses[p.ID].Status = StatusDisabled
		m.stopRunLocked(p.ID)
	}

	m.AddLog(fmt.Sprintf("Connection profile saved: %s", p.Label))
	return nil
}

// DeleteConnection deletes a connection profile and stops its running tunnel.
func (m *ConnectionManager) DeleteConnection(id string) error {
	m.mu.Lock()
	p, ok := m.profiles[id]
	if !ok {
		m.mu.Unlock()
		return ErrProfileNotFound
	}
	label := p.Label
	status := m.statuses[id]
	delete(m.profiles, id)
	delete(m.statuses, id)
	err := m.saveProfiles()
	if err != nil {
		m.profiles[id] = p
		m.statuses[id] = status
		m.mu.Unlock()
		return fmt.Errorf("save profiles: %w", err)
	}
	m.stopRunLocked(id)
	m.mu.Unlock()
	m.AddLog(fmt.Sprintf("Deleted connection: %s", label))
	return nil
}

// ToggleConnection enables or disables a connection.
func (m *ConnectionManager) ToggleConnection(ctx context.Context, id string, enabled bool) error {
	m.mu.Lock()
	p, ok := m.profiles[id]
	if !ok {
		m.mu.Unlock()
		return ErrProfileNotFound
	}
	label := p.Label
	oldEnabled := p.Enabled
	oldStatusEnabled := m.statuses[id].Enabled
	oldStatusStatus := m.statuses[id].Status
	p.Enabled = enabled
	m.statuses[id].Enabled = enabled
	if !enabled {
		m.statuses[id].Status = StatusDisabled
	}
	err := m.saveProfiles()
	if err != nil {
		p.Enabled = oldEnabled
		m.statuses[id].Enabled = oldStatusEnabled
		m.statuses[id].Status = oldStatusStatus
		m.mu.Unlock()
		return fmt.Errorf("save profiles: %w", err)
	}
	if !enabled {
		m.stopRunLocked(id)
	}
	m.mu.Unlock()
	if enabled {
		m.AddLog(fmt.Sprintf("Toggled ON connection: %s", label))
		m.startTunnel(ctx, id)
	} else {
		m.AddLog(fmt.Sprintf("Toggled OFF connection: %s", label))
	}
	return nil
}
