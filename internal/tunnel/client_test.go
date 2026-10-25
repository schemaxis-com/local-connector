package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/schemaxis-com/local-connector/internal/config"
)

// slowExecutor simulates a query that takes longer than the platform's
// heartbeat interval to complete, as a dictionary query on a large schema can.
type slowExecutor struct {
	delay   time.Duration
	started chan struct{}
}

func (e *slowExecutor) Execute(sqlText string, params map[string]any) ([][]any, error) {
	if e.started != nil {
		close(e.started)
	}
	time.Sleep(e.delay)
	return [][]any{{"ok"}}, nil
}

func (e *slowExecutor) ExecuteOne(sqlText string, params map[string]any) ([]any, error) {
	return nil, nil
}

func (e *slowExecutor) ExecuteClob(sqlText string, params map[string]any) (string, error) {
	return "", nil
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// The agent must answer a {"type":"ping"} heartbeat, and so stay inside the
// platform's liveness window, even while a slow query it received earlier is
// still running. Handled inline in the read loop, the query would keep the
// loop from reading the ping until it finished.
func TestPingAnsweredWhileQueryInFlight(t *testing.T) {
	pongReceived := make(chan struct{}, 1)
	queryStarted := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade failed: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()

		// Send a SQL request that the slowExecutor will block on for longer
		// than the ping-response deadline this test enforces below.
		req := map[string]any{
			"id":   "req-1",
			"sql":  "SELECT * FROM employees",
			"mode": "all",
		}
		reqBytes, _ := json.Marshal(req)
		if err := conn.WriteMessage(websocket.TextMessage, reqBytes); err != nil {
			t.Errorf("write request failed: %v", err)
			return
		}

		// Wait until the agent has actually started executing the slow query
		// before sending the heartbeat, so the pong proves the ping was
		// answered while the query ran, not before it.
		<-queryStarted

		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"ping"}`)); err != nil {
			t.Errorf("write ping failed: %v", err)
			return
		}

		// Read messages until the pong arrives (the query's own response
		// message, if it arrives first, is ignored here).
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			_ = conn.SetReadDeadline(time.Now().Add(1 * time.Second))
			_, msg, err := conn.ReadMessage()
			if err != nil {
				continue
			}
			var envelope map[string]any
			if err := json.Unmarshal(msg, &envelope); err != nil {
				continue
			}
			if envelope["type"] == "pong" {
				select {
				case pongReceived <- struct{}{}:
				default:
				}
				return
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + server.URL[len("http"):]

	cfg := &config.Config{
		TunnelURL:  wsURL,
		AgentToken: "test-token",
		OracleUser: "TESTUSER",
	}

	// Query takes 3s to complete; the pong must arrive well before that,
	// proving the read loop was never blocked waiting on it.
	executor := &slowExecutor{delay: 3 * time.Second, started: queryStarted}
	client := NewClient(cfg, executor)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = client.connectAndServe(ctx)
	}()

	select {
	case <-pongReceived:
		// Success: pong answered while the 3s query was still in flight.
	case <-time.After(4 * time.Second):
		t.Fatal("pong not received within 4s: read loop was blocked by the in-flight query")
	}

	cancel()
	wg.Wait()
}

// TestRunStopsWhenReplacedByAnotherAgent pins the takeover path end to end.
// The backend closes the older of two agents sharing a connection_id with
// 1008 and a fixed reason. That close error reaches Run wrapped by
// connectAndServe, and the manager recognises the outcome with errors.Is, so
// both the unwrapping and the sentinel matter: with either one missing, Run
// reconnects and the two agents keep evicting each other.
func TestRunStopsWhenReplacedByAnotherAgent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade failed: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.ClosePolicyViolation, duplicateRejectionReason))
		_, _, _ = conn.ReadMessage()
	}))
	defer server.Close()

	cfg := &config.Config{
		TunnelURL:  "ws" + server.URL[len("http"):],
		AgentToken: "test-token",
		OracleUser: "TESTUSER",
	}
	client := NewClient(cfg, &slowExecutor{})

	// Well under the first reconnect backoff, so a Run that failed to
	// recognise the takeover is still sleeping when the deadline hits.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := client.Run(ctx); !errors.Is(err, ErrDuplicateRejection) {
		t.Fatalf("Run() = %v, want ErrDuplicateRejection", err)
	}
}

// TestRunRefusesPlaintextRemoteURL pins that the token never leaves in clear.
// Without the check, Run would dial, fail on the unresolvable host, and sleep
// through the first reconnect backoff until the deadline, returning nil.
func TestRunRefusesPlaintextRemoteURL(t *testing.T) {
	cfg := &config.Config{
		TunnelURL:  "ws://platform.example.invalid/api/v1/tunnel/connect",
		AgentToken: "test-token",
		OracleUser: "TESTUSER",
	}
	client := NewClient(cfg, &slowExecutor{})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := client.Run(ctx); !errors.Is(err, ErrInvalidURL) {
		t.Fatalf("Run() = %v, want ErrInvalidURL", err)
	}
}

// SQL text reaches the log only while AGENT_LOG_SQL is on. A message the agent
// cannot serve is no exception: it is logged without its text, which may hold
// a query.
func TestUnreadableMessagesAreLoggedWithoutTheirText(t *testing.T) {
	t.Setenv("AGENT_LOG_SQL", "")
	const query = "SELECT salary FROM payroll"
	unreadable := []string{
		`not json, ` + query,
		`{"id": 7, "sql": "` + query + `", "mode": "all"}`, // an ID that is not a string
		`{"sql": "` + query + `", "mode": "all"}`,          // no ID at all
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade failed: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		for _, msg := range append(unreadable, `{"type":"ping"}`) {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
				t.Errorf("write failed: %v", err)
				return
			}
		}
		// The pong comes after the three messages have been read and logged.
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil || string(msg) == `{"type":"pong"}` {
				return
			}
		}
	}))
	defer server.Close()

	var mu sync.Mutex
	var logged []string
	client := fastClient(wsURL(server), &countingExecutor{}).WithLogger(func(msg string) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, msg)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = client.connectAndServe(ctx)

	mu.Lock()
	defer mu.Unlock()
	ignored := 0
	for _, line := range logged {
		if strings.Contains(line, "payroll") {
			t.Errorf("the log carries the SQL text: %q", line)
		}
		if strings.HasPrefix(line, "Ignored ") {
			ignored++
		}
	}
	if ignored != len(unreadable) {
		t.Fatalf("%d messages logged as ignored, want %d: %q", ignored, len(unreadable), logged)
	}
}

// answerExecutor answers every mode with the values it holds.
type answerExecutor struct {
	all  [][]any
	one  []any
	clob string
	err  error
}

func (e *answerExecutor) Execute(string, map[string]any) ([][]any, error)  { return e.all, e.err }
func (e *answerExecutor) ExecuteOne(string, map[string]any) ([]any, error) { return e.one, e.err }
func (e *answerExecutor) ExecuteClob(string, map[string]any) (string, error) {
	return e.clob, e.err
}

// What the platform receives for each mode, byte for byte: rows for every
// answer, null for "one" with no row, and an error rather than rows when the
// query failed.
func TestEachModeIsAnsweredAsThePlatformReadsIt(t *testing.T) {
	cases := []struct {
		name     string
		mode     string
		executor *answerExecutor
		want     string
	}{
		{"all", "all", &answerExecutor{all: [][]any{{"HR", int64(1)}, {"SCOTT", nil}}}, `{"id":"q1","rows":[["HR",1],["SCOTT",null]]}`},
		{"all, no row", "all", &answerExecutor{all: [][]any{}}, `{"id":"q1","rows":[]}`},
		{"no mode reads all rows", "", &answerExecutor{all: [][]any{{"HR"}}}, `{"id":"q1","rows":[["HR"]]}`},
		{"one", "one", &answerExecutor{one: []any{"HR", "EMPLOYEES"}}, `{"id":"q1","rows":["HR","EMPLOYEES"]}`},
		{"one, no row", "one", &answerExecutor{}, `{"id":"q1","rows":null}`},
		{"clob", "clob", &answerExecutor{clob: "CREATE TABLE t (x NUMBER)"}, `{"id":"q1","rows":"CREATE TABLE t (x NUMBER)"}`},
		{"clob, no value", "clob", &answerExecutor{}, `{"id":"q1","rows":""}`},
		{"failed query", "all", &answerExecutor{err: errors.New("ORA-00942: table or view does not exist")}, `{"id":"q1","error":"ORA-00942: table or view does not exist"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := NewClient(&config.Config{}, tc.executor).handleRequest(Request{ID: "q1", SQL: "SELECT 1 FROM dual", Mode: tc.mode})
			got, err := json.Marshal(resp)
			if err != nil {
				t.Fatalf("the response cannot be encoded: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
}

// A value JSON cannot carry is answered as an error. Sent without rows and
// without an error, the answer would read as a query that found nothing.
func TestAResultJSONCannotCarryIsAnsweredAsAnError(t *testing.T) {
	cases := []struct {
		name     string
		mode     string
		executor *answerExecutor
	}{
		{"a NaN", "all", &answerExecutor{all: [][]any{{"HR", math.NaN()}}}},
		{"a date before the year 0", "one", &answerExecutor{one: []any{time.Date(-44, time.March, 15, 0, 0, 0, 0, time.UTC)}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := NewClient(&config.Config{}, tc.executor).handleRequest(Request{ID: "q1", SQL: "SELECT 1 FROM dual", Mode: tc.mode})
			if resp.Rows != nil || !strings.HasPrefix(resp.Error, "the result cannot be sent as JSON: ") {
				t.Fatalf("got rows %s and error %q, want no rows and the reason", resp.Rows, resp.Error)
			}
		})
	}
}
