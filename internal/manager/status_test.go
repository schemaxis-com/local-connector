package manager

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/schemaxis-com/local-connector/internal/config"
	"github.com/schemaxis-com/local-connector/internal/proxy"
	"github.com/sijms/go-ora/v2/network"
)

// databaseSwitch is the state of a fake Oracle the test turns on and off, or
// makes refuse the login or the network encryption.
type databaseSwitch struct{ up, refuse, refuseEncryption atomic.Bool }

var errLoginRefused = fmt.Errorf("oracle ping: %w",
	&network.OracleError{ErrCode: 1017, ErrMsg: "ORA-01017: invalid credential or not authorized; logon denied"})

var errEncryptionRefused = fmt.Errorf("oracle ping: %w", &proxy.EncryptionRefusedError{Code: 12269})

// fakeDatabase is an open connection to that fake Oracle.
type fakeDatabase struct {
	sw         *databaseSwitch
	lastAnswer atomic.Pointer[time.Time]
}

func (d *fakeDatabase) Execute(string, map[string]any) ([][]any, error) {
	return nil, nil
}

func (d *fakeDatabase) ExecuteOne(string, map[string]any) ([]any, error) {
	return nil, nil
}
func (d *fakeDatabase) ExecuteClob(string, map[string]any) (string, error) { return "", nil }
func (d *fakeDatabase) Close() error                                       { return nil }

func (d *fakeDatabase) Ping(context.Context) error {
	if d.sw.refuse.Load() {
		return errLoginRefused
	}
	if d.sw.refuseEncryption.Load() {
		return errEncryptionRefused
	}
	if !d.sw.up.Load() {
		return errors.New("oracle ping: ORA-12541: TNS:no listener")
	}
	now := time.Now()
	d.lastAnswer.Store(&now)
	return nil
}

func (d *fakeDatabase) LastAnswer() time.Time {
	if t := d.lastAnswer.Load(); t != nil {
		return *t
	}
	return time.Time{}
}

// useFakeDatabase makes every connection open the fake Oracle behind sw, with
// the waits shrunk to milliseconds, for the rest of the test.
func useFakeDatabase(t *testing.T, sw *databaseSwitch) {
	t.Helper()
	useOpenDatabase(t, func(context.Context, *config.Config) (database, error) {
		if !sw.up.Load() {
			return nil, errors.New("oracle ping: ORA-12541: TNS:no listener")
		}
		return &fakeDatabase{sw: sw}, nil
	})
}

func useOpenDatabase(t *testing.T, open func(context.Context, *config.Config) (database, error)) {
	t.Helper()
	savedOpen, savedRetry, savedInterval := openDatabase, databaseRetryDelay, databaseCheckInterval
	openDatabase, databaseRetryDelay, databaseCheckInterval = open, 10*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() {
		openDatabase, databaseRetryDelay, databaseCheckInterval = savedOpen, savedRetry, savedInterval
	})
}

var testUpgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

// platform stands in for the platform's tunnel endpoint: each connection is
// served with serve, or held open until the agent closes it.
func platform(t *testing.T, serve func(*websocket.Conn)) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		if serve != nil {
			serve(conn)
			return
		}
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	return "ws" + server.URL[len("http"):]
}

// runningManager returns a manager holding one enabled profile on tunnelURL,
// whose tunnels are stopped and waited for when the test ends.
func runningManager(t *testing.T, tunnelURL string) *ConnectionManager {
	t.Helper()
	mgr := newTestManager(t)
	p := testProfile(true)
	p.TunnelURL = tunnelURL
	if err := mgr.SaveConnection(p); err != nil {
		t.Fatalf("save: %v", err)
	}
	t.Cleanup(func() {
		mgr.StopAll()
		mgr.running.Wait()
	})
	return mgr
}

func status(mgr *ConnectionManager) *ConnectionStatus {
	return mgr.GetConnections()[0]
}

// waitForStatus waits until the profile's status is want and its detail
// contains detail.
func waitForStatus(t *testing.T, mgr *ConnectionManager, want, detail string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s := status(mgr); s.Status == want && strings.Contains(s.LastError, detail) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	s := status(mgr)
	t.Fatalf("status = %q (%q), want %q with %q in its detail", s.Status, s.LastError, want, detail)
}

func countLogs(mgr *ConnectionManager, substr string) int {
	n := 0
	for _, e := range mgr.GetLogs() {
		if strings.Contains(e.Message, substr) {
			n++
		}
	}
	return n
}

func TestLinkStatus(t *testing.T) {
	cases := []struct {
		name       string
		run        tunnelRun
		wantStatus string
		wantDetail string
	}{
		{"both up", tunnelRun{tunnel: tunnelUp}, StatusConnected, ""},
		{"database down under a live tunnel", tunnelRun{tunnel: tunnelUp, dbErr: "ORA-12541: TNS:no listener"}, StatusReconnecting,
			"The database is not answering: ORA-12541: TNS:no listener. The tunnel is up, so queries from the platform fail until the database is back"},
		{"tunnel opening", tunnelRun{tunnel: tunnelOpening}, StatusReconnecting, "Database connected. Establishing tunnel..."},
		{"tunnel opening, database down", tunnelRun{tunnel: tunnelOpening, dbErr: "ORA-12541"}, StatusReconnecting,
			"The database is not answering: ORA-12541. Establishing tunnel..."},
		{"tunnel down", tunnelRun{tunnel: tunnelDown, tunnelErr: "Tunnel not connected (attempt 2): EOF"}, StatusReconnecting,
			"Tunnel not connected (attempt 2): EOF"},
		{"both down", tunnelRun{tunnel: tunnelDown, tunnelErr: "Tunnel not connected (attempt 2): EOF", dbErr: "ORA-12541"}, StatusReconnecting,
			"The database is not answering: ORA-12541. Tunnel not connected (attempt 2): EOF"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, detail := tc.run.linkStatus()
			if status != tc.wantStatus || detail != tc.wantDetail {
				t.Errorf("linkStatus() = (%q, %q), want (%q, %q)", status, detail, tc.wantStatus, tc.wantDetail)
			}
		})
	}
}

// A connections.json edited by hand can hold a tunnel URL with spaces around
// it. The URL check accepts it, so the tunnel must dial it too, rather than
// retry for good on an address the dialer cannot parse.
func TestATunnelURLWithSpacesAroundItConnects(t *testing.T) {
	sw := &databaseSwitch{}
	sw.up.Store(true)
	useFakeDatabase(t, sw)
	mgr := runningManager(t, "  "+platform(t, nil)+" ")

	mgr.startTunnel(context.Background(), "conn_1")
	waitForStatus(t, mgr, StatusConnected, "")
}

// A run that was stopped or replaced may still be finishing something, and
// what it reports then is not true of the profile any more.
func TestOnlyTheCurrentRunChangesTheStatus(t *testing.T) {
	mgr := newTestManager(t)
	if err := mgr.SaveConnection(testProfile(false)); err != nil {
		t.Fatalf("save: %v", err)
	}
	stale, current := &tunnelRun{cancel: func() {}}, &tunnelRun{cancel: func() {}}
	mgr.runs["conn_1"] = current

	mgr.setStatus(stale, "conn_1", StatusReconnecting, "stale")
	if mgr.setLinks(stale, "conn_1", func(r *tunnelRun) { r.tunnel = tunnelUp }) {
		t.Error("setLinks accepted a run that is not the current one")
	}
	if s := status(mgr); s.Status != StatusDisabled {
		t.Fatalf("a stale run changed the status to %q (%q)", s.Status, s.LastError)
	}

	mgr.setLinks(current, "conn_1", func(r *tunnelRun) { r.tunnel = tunnelUp })
	if s := status(mgr); s.Status != StatusConnected {
		t.Fatalf("the current run did not change the status: %q", s.Status)
	}
}

// The tunnel connects once the database is up, and the card must not stay
// green when the database then goes away while the tunnel stays up: the
// status follows both links, not the tunnel alone.
func TestTheStatusFollowsTheDatabaseWhileTheTunnelIsUp(t *testing.T) {
	sw := &databaseSwitch{}
	useFakeDatabase(t, sw)
	var reportsMu sync.Mutex
	var reports []string
	mgr := runningManager(t, platform(t, func(conn *websocket.Conn) {
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if strings.Contains(string(msg), `"database_status"`) {
				reportsMu.Lock()
				reports = append(reports, string(msg))
				reportsMu.Unlock()
			}
		}
	}))

	mgr.startTunnel(context.Background(), "conn_1")
	waitForStatus(t, mgr, StatusReconnecting, "Oracle connect failed")

	sw.up.Store(true)
	waitForStatus(t, mgr, StatusConnected, "")

	sw.up.Store(false)
	waitForStatus(t, mgr, StatusReconnecting, "The database is not answering: oracle ping: ORA-12541")
	time.Sleep(10 * databaseCheckInterval) // more checks, all failing

	sw.up.Store(true)
	waitForStatus(t, mgr, StatusConnected, "")

	// The platform was told too: yes on open, no, then yes again.
	reportsMu.Lock()
	got := strings.Join(reports, " ")
	reportsMu.Unlock()
	want := `{"answering":true,"type":"database_status"} {"answering":false,"type":"database_status"} {"answering":true,"type":"database_status"}`
	if got != want {
		t.Errorf("the platform received %s, want %s", got, want)
	}

	// Each change is logged once, not once per check.
	if n := countLogs(mgr, "not answering"); n != 1 {
		t.Errorf("%d log lines say the database is not answering, want 1", n)
	}
	if n := countLogs(mgr, "The database is answering again"); n != 1 {
		t.Errorf("%d log lines say the database is answering again, want 1", n)
	}
}

// A run stopped while its Oracle connect is still pending, as when the user
// fixes a profile and saves it, must leave the replacement's status alone.
// Otherwise the old run finishes its attempt later and turns the card first to
// Reconnecting, with the old profile's error, and then for good to a
// Connecting... that nothing is doing.
func TestAStoppedRunLeavesItsReplacementAlone(t *testing.T) {
	sw := &databaseSwitch{}
	sw.up.Store(true)
	release := make(chan struct{})
	var releaseOnce sync.Once
	var opens atomic.Int32
	useOpenDatabase(t, func(context.Context, *config.Config) (database, error) {
		if opens.Add(1) == 1 {
			<-release // a connect that does not notice it was cancelled
			return nil, errors.New("ORA-12170: TNS:Connect timeout occurred")
		}
		return &fakeDatabase{sw: sw}, nil
	})
	mgr := runningManager(t, platform(t, nil))
	// Registered last so it runs first: the manager's cleanup waits for the
	// run blocked above.
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	mgr.startTunnel(context.Background(), "conn_1")
	for opens.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	mgr.startTunnel(context.Background(), "conn_1") // saving the profile does this
	waitForStatus(t, mgr, StatusConnected, "")

	releaseOnce.Do(func() { close(release) })
	deadline := time.Now().Add(5 * time.Second)
	for countLogs(mgr, "Tunnel stopped") == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if countLogs(mgr, "Tunnel stopped") == 0 {
		t.Fatal("the stopped run never ended")
	}

	if s := status(mgr); s.Status != StatusConnected {
		t.Errorf("status = %q (%q), want %q: the stopped run overwrote it", s.Status, s.LastError, StatusConnected)
	}
	if n := countLogs(mgr, "Oracle connect failed"); n != 0 {
		t.Errorf("the stopped run logged its late connect failure as news (%d lines)", n)
	}
}

// When another agent takes the connection over, this one stops for good, and
// the card must say so. Marked disabled while still enabled, it would show as
// Connecting... with no message.
func TestATakeoverIsShownAsAnError(t *testing.T) {
	sw := &databaseSwitch{}
	sw.up.Store(true)
	useFakeDatabase(t, sw)
	mgr := runningManager(t, platform(t, func(conn *websocket.Conn) {
		_ = conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "Another agent connected with the same connection_id"))
		_, _, _ = conn.ReadMessage()
	}))

	mgr.startTunnel(context.Background(), "conn_1")
	waitForStatus(t, mgr, StatusError, "Another agent connected with this connection's token")
	if n := countLogs(mgr, "Another agent connected"); n != 1 {
		t.Errorf("%d log lines report the takeover, want 1", n)
	}
}

// probe is a database whose ping the test scripts.
type probe struct {
	fakeDatabase
	ping  func(ctx context.Context) error
	pings atomic.Int32
}

func (p *probe) Ping(ctx context.Context) error {
	p.pings.Add(1)
	return p.ping(ctx)
}

func (p *probe) answeredNow() {
	now := time.Now()
	p.lastAnswer.Store(&now)
}

func TestCheckDatabase(t *testing.T) {
	refused := errors.New("oracle ping: dial tcp 127.0.0.1:1521: connect: connection refused")

	t.Run("a statement answered since the last check needs no ping", func(t *testing.T) {
		since := time.Now().Add(-time.Second)
		p := &probe{ping: func(context.Context) error { return refused }}
		p.answeredNow()
		if err := checkDatabase(context.Background(), p, since); err != nil || p.pings.Load() != 0 {
			t.Fatalf("checkDatabase = %v after %d ping(s), want nil without a ping", err, p.pings.Load())
		}
	})

	t.Run("a failed ping is reported", func(t *testing.T) {
		p := &probe{ping: func(context.Context) error { return refused }}
		if err := checkDatabase(context.Background(), p, time.Now()); !errors.Is(err, refused) {
			t.Fatalf("checkDatabase = %v, want %v", err, refused)
		}
	})

	// All sessions busy with queries: the ping waits, and gives up, while
	// one of them completes. The database answered, so it is not down.
	t.Run("a ping that timed out while a query completed is not believed", func(t *testing.T) {
		p := &probe{}
		p.ping = func(context.Context) error {
			p.answeredNow()
			return context.DeadlineExceeded
		}
		if err := checkDatabase(context.Background(), p, time.Now()); err != nil {
			t.Fatalf("checkDatabase = %v, want nil", err)
		}
	})

	t.Run("a ping that times out says so plainly", func(t *testing.T) {
		p := &probe{ping: func(context.Context) error { return context.DeadlineExceeded }}
		err := checkDatabase(context.Background(), p, time.Now())
		if err == nil || !strings.HasPrefix(err.Error(), "no answer within ") {
			t.Fatalf("checkDatabase = %v, want a plain timeout", err)
		}
	})
}

// A login the database refuses must not be retried. Oracle locks the account
// after a few failed logins, ten by default, so retrying every five seconds
// would lock it in under a minute.
func TestARefusedLoginIsNotRetried(t *testing.T) {
	var opens atomic.Int32
	useOpenDatabase(t, func(context.Context, *config.Config) (database, error) {
		opens.Add(1)
		return nil, errLoginRefused
	})
	mgr := runningManager(t, platform(t, nil))

	mgr.startTunnel(context.Background(), "conn_1")
	waitForStatus(t, mgr, StatusError, "The database refused the login: oracle ping: ORA-01017")
	time.Sleep(10 * databaseRetryDelay)

	if n := opens.Load(); n != 1 {
		t.Errorf("%d logins tried, want 1", n)
	}
}

// The same while the tunnel runs, as after a password change once the pool
// needs a new session: the connection stops, its tunnel closes, and the card
// says why.
func TestALoginRefusedWhileRunningStopsTheConnection(t *testing.T) {
	sw := &databaseSwitch{}
	sw.up.Store(true)
	useFakeDatabase(t, sw)
	closed := make(chan struct{})
	mgr := runningManager(t, platform(t, func(conn *websocket.Conn) {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				close(closed)
				return
			}
		}
	}))

	mgr.startTunnel(context.Background(), "conn_1")
	waitForStatus(t, mgr, StatusConnected, "")
	sw.refuse.Store(true)
	waitForStatus(t, mgr, StatusError, "The database refused the login")

	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the tunnel stayed open after the login was refused")
	}
	time.Sleep(10 * databaseCheckInterval)
	if s := status(mgr); s.Status != StatusError {
		t.Errorf("status = %q (%q) after the stop, want it to stay %q", s.Status, s.LastError, StatusError)
	}
}

// A network encryption the database refuses must not be retried either. The
// refusal repeats until the database's sqlnet.ora changes, and the database
// writes each one to its alert log: retrying every five seconds would add two
// lines there for as long as the agent ran.
func TestARefusedEncryptionIsNotRetried(t *testing.T) {
	var opens atomic.Int32
	useOpenDatabase(t, func(context.Context, *config.Config) (database, error) {
		opens.Add(1)
		return nil, errEncryptionRefused
	})
	mgr := runningManager(t, platform(t, nil))

	mgr.startTunnel(context.Background(), "conn_1")
	waitForStatus(t, mgr, StatusError,
		"The database refused the connection while setting up network encryption: oracle ping: ORA-12269")
	time.Sleep(10 * databaseRetryDelay)

	if n := opens.Load(); n != 1 {
		t.Errorf("%d connections tried, want 1", n)
	}
	if s := status(mgr); strings.Contains(s.LastError, "credentials") {
		t.Errorf("status detail %q talks about credentials, which the refusal has nothing to do with", s.LastError)
	}
}

// The same while the tunnel runs, as when the pool needs a new session after
// the database's network settings were tightened: the connection stops and
// its tunnel closes.
func TestAnEncryptionRefusedWhileRunningStopsTheConnection(t *testing.T) {
	sw := &databaseSwitch{}
	sw.up.Store(true)
	useFakeDatabase(t, sw)
	closed := make(chan struct{})
	mgr := runningManager(t, platform(t, func(conn *websocket.Conn) {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				close(closed)
				return
			}
		}
	}))

	mgr.startTunnel(context.Background(), "conn_1")
	waitForStatus(t, mgr, StatusConnected, "")
	sw.refuseEncryption.Store(true)
	waitForStatus(t, mgr, StatusError, "while setting up network encryption")

	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the tunnel stayed open after the network encryption was refused")
	}
}
