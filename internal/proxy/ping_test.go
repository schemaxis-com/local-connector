package proxy

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/sijms/go-ora/v2/network"
)

// restartingDB is a database the test can stop, start and restart. A restart
// kills every session opened before it, as Oracle does, and the pool only
// finds that out when it uses one.
type restartingDB struct {
	mu     sync.Mutex
	up     bool
	boot   int  // sessions opened in an earlier boot are dead
	refuse bool // refuse every login, as for a changed password
	logins int  // login attempts
}

func (d *restartingDB) restart(up bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.boot++
	d.up = up
}

func (d *restartingDB) Connect(context.Context) (driver.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.up {
		return nil, errors.New("dial tcp 127.0.0.1:1521: connect: connection refused")
	}
	d.logins++
	if d.refuse {
		return nil, &network.OracleError{ErrCode: 1017, ErrMsg: "ORA-01017: invalid credential or not authorized; logon denied"}
	}
	return &restartingSession{db: d, boot: d.boot}, nil
}

func (d *restartingDB) Driver() driver.Driver { return nil }

type restartingSession struct {
	db   *restartingDB
	boot int
}

func (s *restartingSession) Ping(context.Context) error {
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	if s.boot != s.db.boot {
		// What go-ora returns when the write to a dropped session fails: a
		// plain network error, not driver.ErrBadConn.
		return errors.New("write tcp 127.0.0.1:62615->127.0.0.1:1521: wsasend: An established connection was aborted")
	}
	return nil
}

func (s *restartingSession) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("not supported")
}
func (s *restartingSession) Close() error              { return nil }
func (s *restartingSession) Begin() (driver.Tx, error) { return nil, errors.New("not supported") }

// executorWithIdleSessions returns an executor on d whose pool holds as many
// idle sessions as it keeps.
func executorWithIdleSessions(t *testing.T, d *restartingDB) *Executor {
	t.Helper()
	db := sql.OpenDB(d)
	db.SetMaxOpenConns(maxOpenConns)
	db.SetMaxIdleConns(maxIdleConns)
	t.Cleanup(func() { _ = db.Close() })

	var sessions []*sql.Conn
	for range maxIdleConns {
		c, err := db.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, c)
	}
	for _, c := range sessions {
		_ = c.Close()
	}
	return &Executor{db: db}
}

// After a database restart the idle sessions in the pool are dead. The
// database answers, so the ping must too, whatever it meets first. A plain
// PingContext returns the dead session to the pool, and every later ping
// fails on it again.
func TestPingAfterARestartGetsPastTheDeadSessions(t *testing.T) {
	d := &restartingDB{up: true}
	e := executorWithIdleSessions(t, d)
	d.restart(true)

	if err := e.Ping(context.Background()); err != nil {
		t.Fatalf("Ping with the database back up = %v, want nil", err)
	}
	if e.LastAnswer().IsZero() {
		t.Error("a ping that succeeded is not recorded as an answer")
	}
}

// With the database down, the ping reports why a session cannot be opened,
// not the dead sessions it discarded on the way.
func TestPingReportsWhyTheDatabaseIsDown(t *testing.T) {
	d := &restartingDB{up: true}
	e := executorWithIdleSessions(t, d)
	d.restart(false)

	err := e.Ping(context.Background())
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("Ping with the database down = %v, want the reason no session opens", err)
	}
	if !e.LastAnswer().IsZero() {
		t.Error("a failed ping is recorded as an answer")
	}
}

func TestIsLoginRefused(t *testing.T) {
	for code, want := range map[int]bool{1017: true, 28000: true, 28001: true, 1045: true, 1005: true, 12541: false, 1033: false, 942: false} {
		err := fmt.Errorf("oracle ping: %w", &network.OracleError{ErrCode: code})
		if got := IsLoginRefused(err); got != want {
			t.Errorf("IsLoginRefused(ORA-%05d) = %v, want %v", code, got, want)
		}
	}
	if IsLoginRefused(errors.New("dial tcp: connection refused")) {
		t.Error("a network error is taken for a refused login")
	}
}

// A refused login is not tried again by this executor: Oracle counts each
// ORA-01017 against the account and locks it after a few.
func TestAfterARefusedLoginNoFurtherLoginIsTried(t *testing.T) {
	d := &restartingDB{up: true}
	e := executorWithIdleSessions(t, d)
	d.restart(true) // the idle sessions die, so a new login is needed
	d.mu.Lock()
	d.refuse, d.logins = true, 0
	d.mu.Unlock()

	if err := e.Ping(context.Background()); !IsLoginRefused(err) {
		t.Fatalf("Ping = %v, want the refused login", err)
	}
	if err := e.Ping(context.Background()); !IsLoginRefused(err) {
		t.Fatalf("second Ping = %v, want the refused login again", err)
	}
	if _, err := e.getConn(context.Background()); !IsLoginRefused(err) {
		t.Fatalf("getConn = %v, want the refused login", err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.logins != 1 {
		t.Fatalf("%d logins tried, want 1", d.logins)
	}
}
