// Package tunnel implements the WSS client that connects to the cloud backend.
package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/schemaxis-com/local-connector/internal/branding"
	"github.com/schemaxis-com/local-connector/internal/config"
)

// QueryExecutor is the SQL execution surface the tunnel needs. Satisfied by
// *proxy.Executor in production; an interface so tests can substitute a
// slow/failing executor without a real Oracle connection.
type QueryExecutor interface {
	Execute(sqlText string, params map[string]any) ([][]any, error)
	ExecuteOne(sqlText string, params map[string]any) ([]any, error)
	ExecuteClob(sqlText string, params map[string]any) (string, error)
}

// Request is a SQL query message received from the cloud backend.
type Request struct {
	ID     string         `json:"id"`
	SQL    string         `json:"sql"`
	Params map[string]any `json:"params,omitempty"`
	Mode   string         `json:"mode"` // "all", "one", "clob"
}

// Response is the result sent back to the cloud backend.
type Response struct {
	ID    string          `json:"id"`
	Rows  json.RawMessage `json:"rows,omitempty"`
	Error string          `json:"error,omitempty"`
}

// OnDisconnectFunc is called when the tunnel fails to open or drops and a
// reconnect is scheduled, with the reason and the number of consecutive
// failures so far. The manager uses this to update the UI status.
type OnDisconnectFunc func(err error, attempt int)

// OnConnectFunc is called when the tunnel successfully connects to the backend.
type OnConnectFunc func()

// Default reconnect timing: the delay doubles from backoffBase on each
// consecutive failure up to backoffMax, and the count starts again only after
// a connection that stayed up for stableWindow.
const (
	defaultBackoffBase  = 5 * time.Second
	defaultBackoffMax   = 60 * time.Second
	defaultStableWindow = 2 * time.Minute
)

// Client manages the WSS connection to the cloud backend.
type Client struct {
	cfg          *config.Config
	executor     QueryExecutor
	logFunc      func(string)     // optional: routes log messages to the manager's UI buffer
	onConnect    OnConnectFunc    // optional: called on successful connection
	onDisconnect OnDisconnectFunc // optional: called before each reconnect

	backoffBase  time.Duration
	backoffMax   time.Duration
	stableWindow time.Duration

	// databaseAnswering is the last state given to ReportDatabase, sent to
	// the platform when the tunnel opens and each time it changes.
	databaseAnswering atomic.Bool
	databaseChanged   chan struct{}
}

// NewClient creates a new tunnel client.
func NewClient(cfg *config.Config, executor QueryExecutor) *Client {
	c := &Client{
		cfg:             cfg,
		executor:        executor,
		backoffBase:     defaultBackoffBase,
		backoffMax:      defaultBackoffMax,
		stableWindow:    defaultStableWindow,
		databaseChanged: make(chan struct{}, 1),
	}
	c.databaseAnswering.Store(true)
	return c
}

// ReportDatabase tells the platform whether the database answers, so that it
// does not offer a connection whose database is down as ready for analysis.
// Only that yes or no leaves the agent, never the reason.
func (c *Client) ReportDatabase(answering bool) {
	if c.databaseAnswering.Swap(answering) == answering {
		return
	}
	select {
	case c.databaseChanged <- struct{}{}:
	default:
	}
}

// WithLogger attaches a log function so tunnel events are routed to the manager's UI buffer.
func (c *Client) WithLogger(logFunc func(string)) *Client {
	c.logFunc = logFunc
	return c
}

// WithOnConnect attaches a callback invoked on successful connection.
func (c *Client) WithOnConnect(fn OnConnectFunc) *Client {
	c.onConnect = fn
	return c
}

// WithOnDisconnect attaches a callback invoked before each reconnect.
func (c *Client) WithOnDisconnect(fn OnDisconnectFunc) *Client {
	c.onDisconnect = fn
	return c
}

// logf routes a formatted message through logFunc if set, otherwise to the
// standard logger. Not both: the manager's log function already writes each
// message to the console as well as to the web UI's log.
func (c *Client) logf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if c.logFunc != nil {
		c.logFunc(msg)
		return
	}
	log.Printf("[%s] %s", c.cfg.OracleUser, msg)
}

// Run connects to the tunnel endpoint and processes queries until ctx is cancelled.
// It automatically reconnects on disconnection with exponential backoff.
// The only exception is a "duplicate rejection", when the server closes the
// connection because another agent with the same connection_id connected. Run
// then returns ErrDuplicateRejection instead of reconnecting, so the two agents
// do not keep taking the tunnel from each other.
func (c *Client) Run(ctx context.Context) error {
	// A URL that can never be dialled safely is a configuration error, not a
	// dropped connection, so it is refused once instead of retried forever.
	if err := ValidateURL(c.cfg.TunnelURL); err != nil {
		c.logf("Tunnel not started: %v", err)
		return err
	}

	// attempt counts consecutive failures and sets the delay. A dial that
	// fails, or a connection that drops soon after it opened, continues the
	// count, so a tunnel that keeps failing backs off to backoffMax instead of
	// retrying at the base delay forever.
	attempt := 0
	for {
		if ctx.Err() != nil {
			return nil
		}

		openedAt, err := c.connectAndServe(ctx)

		if ctx.Err() != nil {
			return nil // graceful shutdown
		}

		// Duplicate rejection: another agent replaced us, so exit immediately
		if isDuplicateRejection(err) {
			c.logf("Another agent connected with this connection's token and took the tunnel over, so this one stopped reconnecting. Only one agent can hold a connection at a time")
			return ErrDuplicateRejection
		}

		if !openedAt.IsZero() && time.Since(openedAt) >= c.stableWindow {
			attempt = 0
		}
		attempt++
		delay := backoff(attempt, c.backoffBase, c.backoffMax)

		if c.onDisconnect != nil {
			c.onDisconnect(err, attempt)
		}
		seconds := int(delay.Round(time.Second) / time.Second)
		if openedAt.IsZero() {
			c.logf("Could not open the tunnel (attempt %d): %v. Retrying in %ds", attempt, err, seconds)
		} else {
			c.logf("Tunnel disconnected (attempt %d): %v. Reconnecting in %ds", attempt, err, seconds)
		}

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

// duplicateRejectionReason is the close reason the backend sends to the older
// of two agents holding the same connection_id.
const duplicateRejectionReason = "Another agent connected with the same connection_id"

// isDuplicateRejection checks if the server closed our connection because
// another agent connected with the same connection_id. The close error arrives
// wrapped by connectAndServe, so it is unwrapped rather than type-asserted.
func isDuplicateRejection(err error) bool {
	var wsErr *websocket.CloseError
	return errors.As(err, &wsErr) &&
		wsErr.Code == websocket.ClosePolicyViolation &&
		wsErr.Text == duplicateRejectionReason
}

// ErrDuplicateRejection is returned by Run when another agent takes over.
var ErrDuplicateRejection = errors.New("replaced by another agent instance")

// connectAndServe opens the tunnel and serves it until it drops or ctx is
// cancelled. openedAt is when the tunnel opened, or the zero time if it never
// did.
func (c *Client) connectAndServe(ctx context.Context) (openedAt time.Time, err error) {
	header := http.Header{}
	header.Set("Authorization", "Bearer "+c.cfg.AgentToken)
	// Tells the platform which release of the agent is connecting.
	header.Set("X-Agent-Version", branding.Version)
	// gorilla/websocket sends no User-Agent of its own, and proxies and bot
	// filters in front of a web service often challenge or refuse a request
	// without one. A WebSocket handshake cannot answer a challenge, so the
	// tunnel would fail to connect with nothing naming the cause. The header
	// identifies the agent and authenticates nothing: the token above does.
	header.Set("User-Agent", branding.AppName+"-agent/"+branding.Version)

	// A copy: DefaultDialer is shared by every connection's goroutine.
	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 15 * time.Second

	conn, resp, err := dialer.DialContext(ctx, c.cfg.TunnelURL, header)
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			return time.Time{}, fmt.Errorf("authentication rejected (HTTP %d): AGENT_TOKEN is invalid or expired, generate a new one: %w", resp.StatusCode, err)
		}
		return time.Time{}, fmt.Errorf("WSS dial failed: %w", err)
	}
	defer func() { _ = conn.Close() }()
	openedAt = time.Now()

	// Stopping the connection closes the tunnel at once. The read below
	// would otherwise wait for the platform's next message, and a query
	// arriving meanwhile would still reach the database of a connection
	// the user has just turned off.
	stopClosing := context.AfterFunc(ctx, func() {
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, "agent shutting down"),
			time.Now().Add(time.Second))
		_ = conn.Close()
	})
	defer stopClosing()

	// Ends the ping loop below with this connection, not only with ctx.
	connDone := make(chan struct{})
	defer close(connDone)

	c.logf("Tunnel connected to %s", c.cfg.TunnelURL)
	if c.onConnect != nil {
		c.onConnect()
	}

	// Each pong extends the read deadline, so a connection that stops
	// answering is detected even while the platform sends nothing.
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	})

	// gorilla/websocket allows at most ONE concurrent writer: the ping loop,
	// heartbeat pongs, and per-query response goroutines all funnel through
	// this mutex-guarded helper.
	var writeMu sync.Mutex
	writeMessage := func(messageType int, data []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteMessage(messageType, data)
	}

	// Ping loop
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-connDone:
				return
			case <-ticker.C:
				if err := writeMessage(websocket.PingMessage, nil); err != nil {
					return
				}
			}
		}
	}()

	// The database's state: once now, since the platform starts every
	// connection assuming the database answers, then on each change.
	go func() {
		send := func() error {
			msg, _ := json.Marshal(map[string]any{
				"type":      "database_status",
				"answering": c.databaseAnswering.Load(),
			})
			return writeMessage(websocket.TextMessage, msg)
		}
		// A change made while the tunnel was down is covered by this send.
		select {
		case <-c.databaseChanged:
		default:
		}
		if send() != nil {
			return
		}
		for {
			select {
			case <-connDone:
				return
			case <-c.databaseChanged:
				if send() != nil {
					return
				}
			}
		}
	}()

	// SQL-text tracing: compile-time default from branding (ships off, see
	// branding.LogSQL for why), overridable per run via AGENT_LOG_SQL for a
	// support session. Same default-plus-env-override shape as the CLIENT_*
	// branding parameters. Read once per connection, not per query.
	logSQL := branding.LogSQL
	if v := os.Getenv("AGENT_LOG_SQL"); v != "" {
		logSQL = v != "0" && !strings.EqualFold(v, "false")
	}

	// Message loop. Each query runs in its own goroutine, so the loop keeps
	// reading and keeps answering the platform's {"type":"ping"} heartbeats
	// while a slow query runs. A heartbeat left unanswered for too long makes
	// the platform close the tunnel.
	for {
		_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))

		_, msg, err := conn.ReadMessage()
		// A message read as the connection was being stopped is not served.
		if ctx.Err() != nil {
			return openedAt, nil
		}
		if err != nil {
			return openedAt, fmt.Errorf("read error: %w", err)
		}

		// A message the agent cannot read is logged without its text, which
		// may hold a query: SQL reaches the log only while logSQL is on.
		var envelope map[string]any
		if err := json.Unmarshal(msg, &envelope); err != nil {
			c.logf("Ignored a message that is not valid JSON (%d bytes): %v", len(msg), err)
			continue
		}

		// Heartbeat: respond to {"type": "ping"} with {"type": "pong"}
		if msgType, ok := envelope["type"].(string); ok && msgType == "ping" {
			if err := writeMessage(websocket.TextMessage, []byte(`{"type":"pong"}`)); err != nil {
				return openedAt, fmt.Errorf("heartbeat response error: %w", err)
			}
			continue
		}

		// Otherwise, it must be a SQL request
		var req Request
		if err := json.Unmarshal(msg, &req); err != nil {
			c.logf("Ignored a request the agent cannot read: %v", err)
			continue
		}

		if req.ID == "" {
			c.logf("Ignored a request without an ID")
			continue
		}

		// Execute query and send response without blocking the read loop.
		// A write failure here only logs: the read loop detects the dead
		// connection itself and drives the reconnect.
		go func(req Request) {
			// Every query is logged with its ID, duration and outcome, so a
			// timeout reported by the platform can be matched with what the
			// database did. The SQL text is logged only while AGENT_LOG_SQL is
			// on (branding.LogSQL says why it ships off).
			started := time.Now()
			if logSQL {
				c.logf("Query %s received: %s", req.ID, snippet(req.SQL))
			} else {
				c.logf("Query %s received", req.ID)
			}
			resp := c.handleRequest(req)
			elapsed := time.Since(started).Round(time.Millisecond)
			if resp.Error != "" {
				c.logf("Query %s failed in %s: %s", req.ID, elapsed, resp.Error)
			} else {
				c.logf("Query %s ok in %s", req.ID, elapsed)
			}
			respBytes, _ := json.Marshal(resp)
			if err := writeMessage(websocket.TextMessage, respBytes); err != nil {
				c.logf("Response write failed for request %s: %v", req.ID, err)
			}
		}(req)
	}
}

// snippet trims and collapses a SQL string to a short single-line form for logs.
func snippet(sql string) string {
	s := strings.Join(strings.Fields(sql), " ")
	if len(s) > 100 {
		return s[:100] + "..."
	}
	return s
}

// handleRequest runs req in the mode it names and returns the answer to send.
// A result JSON cannot carry (a NaN, or a date outside the years 0 to 9999)
// is answered as an error: sent without rows, it would read as an empty
// result.
func (c *Client) handleRequest(req Request) Response {
	var result any
	var err error
	switch req.Mode {
	case "one":
		result, err = c.executor.ExecuteOne(req.SQL, req.Params)
	case "clob":
		result, err = c.executor.ExecuteClob(req.SQL, req.Params)
	default: // "all"
		result, err = c.executor.Execute(req.SQL, req.Params)
	}
	if err != nil {
		return Response{ID: req.ID, Error: err.Error()}
	}
	data, err := json.Marshal(result)
	if err != nil {
		return Response{ID: req.ID, Error: fmt.Sprintf("the result cannot be sent as JSON: %v", err)}
	}
	return Response{ID: req.ID, Rows: data}
}

// backoff calculates exponential backoff with jitter: base * 2^(attempt-1),
// capped at limit, plus or minus 25% to avoid a thundering herd.
func backoff(attempt int, base, limit time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	exp := float64(base) * math.Pow(2, float64(attempt-1))
	if exp > float64(limit) {
		exp = float64(limit)
	}
	jitter := (rand.Float64() - 0.5) * 0.5 * exp //nolint:gosec // non-cryptographic reconnect-delay jitter, not a security context
	return time.Duration(math.Round(exp + jitter))
}
