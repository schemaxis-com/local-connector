package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/schemaxis-com/local-connector/internal/config"
)

// countingExecutor records how many queries reached it.
type countingExecutor struct{ calls atomic.Int32 }

func (e *countingExecutor) Execute(string, map[string]any) ([][]any, error) {
	e.calls.Add(1)
	return [][]any{}, nil
}

func (e *countingExecutor) ExecuteOne(string, map[string]any) ([]any, error) {
	e.calls.Add(1)
	return nil, nil
}

func (e *countingExecutor) ExecuteClob(string, map[string]any) (string, error) {
	e.calls.Add(1)
	return "", nil
}

// fastClient returns a client for url whose reconnect timing is shrunk to
// milliseconds, with the stable window a few delays long, and whose log goes
// nowhere.
func fastClient(url string, executor QueryExecutor) *Client {
	c := NewClient(&config.Config{TunnelURL: url, AgentToken: "test-token", OracleUser: "TESTUSER"}, executor).
		WithLogger(func(string) {})
	c.backoffBase = time.Millisecond
	c.backoffMax = 4 * time.Millisecond
	c.stableWindow = 20 * time.Millisecond
	return c
}

// wsURL is the WebSocket address of a test server.
func wsURL(server *httptest.Server) string {
	return "ws" + server.URL[len("http"):]
}

// recordAttempts runs c until it has scheduled n reconnects and returns the
// attempt number reported for each.
func recordAttempts(t *testing.T, c *Client, n int) []int {
	t.Helper()
	attempts := make(chan int, n)
	c.WithOnDisconnect(func(_ error, attempt int) {
		select {
		case attempts <- attempt:
		default:
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.Run(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()

	var got []int
	timeout := time.After(10 * time.Second)
	for len(got) < n {
		select {
		case a := <-attempts:
			got = append(got, a)
		case <-timeout:
			t.Fatalf("only %d reconnects in 10s: %v", len(got), got)
		}
	}
	return got
}

func wantRising(t *testing.T, got []int) {
	t.Helper()
	for i, a := range got {
		if a != i+1 {
			t.Fatalf("attempts = %v, want a count that keeps rising from 1", got)
		}
	}
}

// A tunnel that cannot open must back off further on every failure. A count
// that started again once two minutes had passed since the run began or the
// last connection opened would make every later failure "attempt 1", and the
// agent would retry at the base delay forever.
func TestAttemptsKeepCountingWhileTheTunnelCannotOpen(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	// Thirty failures take several stable windows.
	wantRising(t, recordAttempts(t, fastClient(wsURL(server), &slowExecutor{}), 30))
}

// A connection that drops as soon as it opens is not a recovery. A count that
// started again after every connection would retry a tunnel accepted and then
// closed at once at the base delay forever.
func TestAttemptsKeepCountingWhenTheTunnelDropsAtOnce(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		_ = conn.Close()
	}))
	defer server.Close()

	wantRising(t, recordAttempts(t, fastClient(wsURL(server), &slowExecutor{}), 10))
}

// A connection that stayed up for the stable window was a recovery, so the
// failure that ends it is the first of a new count.
func TestAttemptsStartAgainAfterAStableConnection(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) != 4 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		time.Sleep(100 * time.Millisecond) // five stable windows
		_ = conn.Close()
	}))
	defer server.Close()

	got := recordAttempts(t, fastClient(wsURL(server), &slowExecutor{}), 5)
	if want := []int{1, 2, 3, 1, 2}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("attempts = %v, want %v", got, want)
	}
}

// serveTunnel serves each tunnel connection with serve.
func serveTunnel(t *testing.T, serve func(*websocket.Conn)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		serve(conn)
	}))
	t.Cleanup(server.Close)
	return server
}

// runConnected runs c and returns once its tunnel is open on the client
// side, with the function that stops it and a channel closed when Run
// returns. Waiting for the client rather than the server matters: the
// server sees the connection before the client has finished opening it.
func runConnected(t *testing.T, c *Client) (stop context.CancelFunc, done <-chan struct{}) {
	t.Helper()
	connected := make(chan struct{}, 1)
	c.WithOnConnect(func() {
		select {
		case connected <- struct{}{}:
		default:
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_ = c.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-finished
	})
	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		t.Fatal("the tunnel never opened")
	}
	return cancel, finished
}

// Turning a connection off must close its tunnel then and there, not when the
// platform next sends something, which on a quiet connection can be ninety
// seconds later while the UI already says Disabled.
func TestStoppingTheClientClosesTheTunnelAtOnce(t *testing.T) {
	closed := make(chan int, 1)
	server := serveTunnel(t, func(conn *websocket.Conn) {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			_, _, err := conn.ReadMessage()
			if err == nil {
				continue // the database state the client sends on open
			}
			var closeErr *websocket.CloseError
			if errors.As(err, &closeErr) {
				closed <- closeErr.Code
			}
			return
		}
	})

	stop, done := runConnected(t, fastClient(wsURL(server), &slowExecutor{}))
	stop()

	select {
	case code := <-closed:
		if code != websocket.CloseNormalClosure {
			t.Errorf("close code = %d, want %d", code, websocket.CloseNormalClosure)
		}
	case <-time.After(time.Second):
		t.Fatal("the platform side did not see the tunnel close within a second of the stop")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return within a second of the stop")
	}
}

// A query the platform sends once the connection has been stopped must not
// reach the database, even when the client reads it before it notices the stop.
func TestAQueryAfterStopIsNotRun(t *testing.T) {
	stopped := make(chan struct{})
	server := serveTunnel(t, func(conn *websocket.Conn) {
		<-stopped
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"id":"late","sql":"SELECT 1 FROM dual","mode":"all"}`))
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})

	executor := &countingExecutor{}
	stop, done := runConnected(t, fastClient(wsURL(server), executor))
	stop()
	close(stopped)
	<-done
	time.Sleep(200 * time.Millisecond) // time for a dispatched query to run

	if n := executor.calls.Load(); n != 0 {
		t.Fatalf("%d query(ies) ran after the connection was stopped", n)
	}
}

// syncBuffer is the console for a test. Goroutines left by earlier tests may
// still write to the standard logger, so it is locked, and a test looks for
// its own lines only.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// With a log function attached, each message goes there only. The manager's
// function also writes to the console, so printing here as well would show
// every tunnel event twice.
func TestTheClientLogsEachMessageOnce(t *testing.T) {
	console := &syncBuffer{}
	saved := log.Writer()
	log.SetOutput(console)
	t.Cleanup(func() { log.SetOutput(saved) })

	var got []string
	c := NewClient(&config.Config{OracleUser: "TESTUSER"}, &slowExecutor{}).
		WithLogger(func(msg string) { got = append(got, msg) })
	c.logf("Tunnel connected to %s", "wss://logged-once.invalid")

	if len(got) != 1 || got[0] != "Tunnel connected to wss://logged-once.invalid" {
		t.Errorf("log function received %q", got)
	}
	if strings.Contains(console.String(), "logged-once") {
		t.Errorf("also printed to the console: %q", console.String())
	}

	NewClient(&config.Config{OracleUser: "TESTUSER"}, &slowExecutor{}).logf("no log function")
	if !strings.Contains(console.String(), "[TESTUSER] no log function") {
		t.Errorf("without a log function nothing reached the console: %q", console.String())
	}
}

func TestBackoffDoublesWithinItsJitterUpToTheLimit(t *testing.T) {
	for attempt, want := range map[int]time.Duration{
		1: 5 * time.Second, 2: 10 * time.Second, 4: 40 * time.Second, 5: 60 * time.Second, 50: 60 * time.Second,
	} {
		for range 20 {
			if got := backoff(attempt, defaultBackoffBase, defaultBackoffMax); got < want*3/4 || got > want*5/4 {
				t.Fatalf("backoff(%d) = %s, want %s plus or minus 25%%", attempt, got, want)
			}
		}
	}
}

// The platform learns whether the database answers: once when the tunnel
// opens, since it starts every connection assuming yes, then on each change,
// and only as a yes or no.
func TestTheDatabaseStateReachesThePlatform(t *testing.T) {
	states := make(chan string, 10)
	server := serveTunnel(t, func(conn *websocket.Conn) {
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var m map[string]any
			if json.Unmarshal(msg, &m) == nil && m["type"] == "database_status" {
				if len(m) != 2 {
					t.Errorf("database_status carries more than its state: %s", msg)
				}
				states <- fmt.Sprint(m["answering"])
			}
		}
	})
	next := func() string {
		select {
		case s := <-states:
			return s
		case <-time.After(2 * time.Second):
			return "nothing"
		}
	}

	c := fastClient(wsURL(server), &slowExecutor{})
	c.ReportDatabase(false) // known before the tunnel opens
	stop, done := runConnected(t, c)
	if got := next(); got != "false" {
		t.Fatalf("on open the platform got %s, want false", got)
	}
	c.ReportDatabase(false) // no change, nothing sent
	c.ReportDatabase(true)
	if got := next(); got != "true" {
		t.Fatalf("after the database came back the platform got %s, want true", got)
	}
	stop()
	<-done
}
