// Package proxy executes SQL queries on the local Oracle database.
package proxy

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/schemaxis-com/local-connector/internal/branding"
	"github.com/schemaxis-com/local-connector/internal/config"
	go_ora "github.com/sijms/go-ora/v2"
	"github.com/sijms/go-ora/v2/network"
)

// QueryTimeout bounds one query, from taking a session to reading its last
// row. A hung database would otherwise hold the query's goroutine forever. It
// stays below the 30 seconds the platform waits for an answer, so a query that
// runs out of time comes back as an error the platform can report, rather than
// as no answer at all.
const QueryTimeout = 25 * time.Second

// The session pool. maxIdleConns also bounds how many dead sessions Ping can
// meet before it reaches a new one.
const (
	maxOpenConns = 5
	maxIdleConns = 2
)

// loginRefusedCodes are the Oracle errors for a login the database refuses
// until someone changes the account or the credentials. Retrying one only
// adds failed logins, and Oracle counts each ORA-01017 against the account's
// FAILED_LOGIN_ATTEMPTS, which locks it (after ten, by default).
var loginRefusedCodes = map[int]bool{
	1005:  true, // null password given
	1017:  true, // invalid username/password
	1045:  true, // user lacks CREATE SESSION privilege
	28000: true, // account is locked
	28001: true, // password has expired
}

// IsLoginRefused reports whether err is a login the database refused.
func IsLoginRefused(err error) bool {
	var oraErr *network.OracleError
	return errors.As(err, &oraErr) && loginRefusedCodes[oraErr.ErrCode]
}

// encryptionRefusals are the Oracle errors that end the network encryption
// negotiation because the database's sqlnet.ora and the agent cannot agree,
// each with what it means. The negotiation comes before the login and the
// database writes each refusal to its alert log, so retrying only adds lines
// there until someone changes the database's network settings.
var encryptionRefusals = map[int]string{
	12269: "the database is set to refuse clients that use the older network encryption negotiation " +
		"(SQLNET.ALLOW_WEAK_CRYPTO_CLIENTS=FALSE), and the agent uses it",
	12649: "the database's sqlnet.ora lists an encryption or checksum algorithm that the database does not support",
	12650: "the database and the agent have no encryption or checksum algorithm in common. " +
		"The agent supports AES256, AES192 and AES128 for encryption, and SHA256, SHA384, SHA512 and SHA1 for checksums",
}

// negotiationError matches the error go-ora returns when the database ends the
// network encryption negotiation with an Oracle error, and captures its number.
var negotiationError = regexp.MustCompile(`(?i)advanced negotiation error:.*\bora-(\d+)`)

// EncryptionRefusedError is a network encryption negotiation the database
// refused, with the reason in words. It wraps the driver's error.
type EncryptionRefusedError struct {
	Code int
	err  error
}

func (e *EncryptionRefusedError) Error() string {
	return fmt.Sprintf("ORA-%05d: %s", e.Code, encryptionRefusals[e.Code])
}

func (e *EncryptionRefusedError) Unwrap() error { return e.err }

// IsEncryptionRefused reports whether err is a network encryption negotiation
// the database refused.
func IsEncryptionRefused(err error) bool {
	var refused *EncryptionRefusedError
	return errors.As(err, &refused)
}

// explainEncryptionRefusal returns err as an EncryptionRefusedError when it is
// go-ora's report of a refused negotiation, and unchanged otherwise.
func explainEncryptionRefusal(err error) error {
	m := negotiationError.FindStringSubmatch(err.Error())
	if m == nil {
		return err
	}
	code, _ := strconv.Atoi(m[1])
	if _, ok := encryptionRefusals[code]; !ok {
		return err
	}
	return &EncryptionRefusedError{Code: code, err: err}
}

// Executor handles SQL execution against the Oracle database.
type Executor struct {
	db *sql.DB
	// lastAnswer is when the database last completed a statement or a ping
	// for this executor. Nil until it has.
	lastAnswer atomic.Pointer[time.Time]
	// refused holds the first login or network encryption the database
	// refused. From then on the executor opens no session: each attempt would
	// meet the same refusal, and a refused login also counts against the
	// account.
	refused atomic.Pointer[refusal]
}

type refusal struct{ err error }

// refusal returns the login or network encryption the database refused
// earlier, if any.
func (e *Executor) refusal() error {
	if r := e.refused.Load(); r != nil {
		return r.err
	}
	return nil
}

// noteRefusal records err if it is a login or a network encryption the
// database refused, and returns it as the executor reports it.
func (e *Executor) noteRefusal(err error) error {
	err = explainEncryptionRefusal(err)
	if IsLoginRefused(err) || IsEncryptionRefused(err) {
		e.refused.CompareAndSwap(nil, &refusal{err: err})
	}
	return err
}

// NewExecutor creates a new Executor connected to Oracle. Cancelling ctx
// abandons the connection attempt.
func NewExecutor(ctx context.Context, cfg *config.Config) (*Executor, error) {
	e := newExecutor(cfg)
	if err := e.Ping(ctx); err != nil {
		_ = e.Close()
		return nil, err
	}
	return e, nil
}

// newExecutor returns an Executor for cfg's database without connecting to it.
//
// The database is opened through a go-ora driver of its own rather than the
// one go-ora registers for the whole process. That driver keeps the character
// set of the first database it connects to and decodes every later connection
// with it, so an agent serving databases in different character sets would
// garble the text of all but the first.
func newExecutor(cfg *config.Config) *Executor {
	db := sql.OpenDB(go_ora.NewConnector(buildConnStr(cfg)))
	db.SetMaxOpenConns(maxOpenConns)
	db.SetMaxIdleConns(maxIdleConns)
	return &Executor{db: db}
}

// Ping checks that the database answers, on a pooled session or a new one.
//
// A pooled session the database dropped while it was idle, as a restart
// does, is only found out when it is used. go-ora marks it bad when a read
// fails but not when a write does, so the pool would hand the same dead
// session to every later ping. Each session a ping fails on is therefore
// discarded and the ping tried again; after maxIdleConns of those the pool
// has none left, and the last try opens a new session, whose error is the
// real reason. After a refused login or network encryption it returns that
// refusal without trying.
func (e *Executor) Ping(ctx context.Context) error {
	if err := e.refusal(); err != nil {
		return fmt.Errorf("oracle ping: %w", err)
	}
	var err error
	for range maxIdleConns + 1 {
		var discarded bool
		if discarded, err = e.pingOnce(ctx); !discarded {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("oracle ping: %w", e.noteRefusal(err))
	}
	e.answered()
	return nil
}

// pingOnce pings on one session and discards it if the ping fails. A session
// that could not be obtained (no session to reuse and none could be opened)
// is not a discard: that error is the answer.
func (e *Executor) pingOnce(ctx context.Context) (discarded bool, err error) {
	conn, err := e.db.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = conn.Close() }()
	var pingErr error
	_ = conn.Raw(func(dc any) error {
		p, ok := dc.(driver.Pinger)
		if !ok {
			return nil
		}
		if pingErr = p.Ping(ctx); pingErr != nil {
			return driver.ErrBadConn // tells database/sql to close the session
		}
		return nil
	})
	return pingErr != nil, pingErr
}

// LastAnswer returns when the database last completed a statement or a ping
// for this executor, or the zero time if it never has.
func (e *Executor) LastAnswer() time.Time {
	if t := e.lastAnswer.Load(); t != nil {
		return *t
	}
	return time.Time{}
}

func (e *Executor) answered() {
	now := time.Now()
	e.lastAnswer.Store(&now)
}

// getConn acquires a connection from the pool and, if PL/SQL session parameters
// (module, action, client info) are configured, initializes the session. After
// a refused login or network encryption it returns that refusal without trying.
func (e *Executor) getConn(ctx context.Context) (*sql.Conn, error) {
	if err := e.refusal(); err != nil {
		return nil, err
	}
	conn, err := e.db.Conn(ctx)
	if err != nil {
		return nil, e.noteRefusal(err)
	}

	module := envOr("CLIENT_MODULE", branding.ClientModule)
	action := envOr("CLIENT_ACTION", branding.ClientAction)
	clientInfo := envOr("CLIENT_INFO", branding.ClientInfo)

	if module != "" || action != "" || clientInfo != "" {
		_, err = conn.ExecContext(ctx, `
			BEGIN
				IF :1 IS NOT NULL OR :2 IS NOT NULL THEN
					DBMS_APPLICATION_INFO.SET_MODULE(:1, :2);
				END IF;
				IF :3 IS NOT NULL THEN
					DBMS_APPLICATION_INFO.SET_CLIENT_INFO(:3);
				END IF;
			END;`, module, action, clientInfo)
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("oracle set session attributes: %w", err)
		}
	}

	return conn, nil
}

// Close closes the database connection.
func (e *Executor) Close() error {
	if e.db != nil {
		return e.db.Close()
	}
	return nil
}

// Execute runs a SELECT query and returns all rows as [][]any.
func (e *Executor) Execute(sqlText string, params map[string]any) ([][]any, error) {
	result := make([][]any, 0)
	err := e.query(sqlText, params, func(rows *sql.Rows, width int) error {
		for rows.Next() {
			row, err := scanRow(rows, width)
			if err != nil {
				return err
			}
			result = append(result, row)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ExecuteOne runs a SELECT query and returns its first row, or nil when it
// has none.
func (e *Executor) ExecuteOne(sqlText string, params map[string]any) ([]any, error) {
	var row []any
	err := e.query(sqlText, params, func(rows *sql.Rows, width int) error {
		if !rows.Next() {
			// Next also returns false when the fetch fails, and that is not
			// an empty result.
			return rows.Err()
		}
		var err error
		row, err = scanRow(rows, width)
		return err
	})
	if err != nil {
		return nil, err
	}
	return row, nil
}

// query checks sqlText, runs it on a pooled session and hands its rows to
// read, which is given the number of columns. QueryTimeout bounds the whole
// call, the reading included. The query counts as an answer from the database
// only when read succeeds.
func (e *Executor) query(sqlText string, params map[string]any, read func(rows *sql.Rows, width int) error) error {
	if err := validateReadOnly(sqlText); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), QueryTimeout)
	defer cancel()

	conn, err := e.getConn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	rows, err := conn.QueryContext(ctx, sqlText, mapToArgs(params)...)
	if err != nil {
		return fmt.Errorf("query error: %w", err)
	}
	defer func() { _ = rows.Close() }()

	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	if err := read(rows, len(cols)); err != nil {
		return err
	}
	e.answered()
	return nil
}

// scanRow reads the current row. Text the driver returns as []byte becomes a
// string, so that it is sent to the platform as text rather than base64.
func scanRow(rows *sql.Rows, width int) ([]any, error) {
	values := make([]any, width)
	scanArgs := make([]any, width)
	for i := range values {
		scanArgs[i] = &values[i]
	}
	if err := rows.Scan(scanArgs...); err != nil {
		return nil, err
	}
	for i, v := range values {
		if b, ok := v.([]byte); ok {
			values[i] = string(b)
		}
	}
	return values, nil
}

// ExecuteClob runs a query expected to return a single CLOB value. It returns
// an empty string when the query has no row or the value is NULL.
func (e *Executor) ExecuteClob(sqlText string, params map[string]any) (string, error) {
	var val sql.NullString
	err := e.query(sqlText, params, func(rows *sql.Rows, _ int) error {
		if !rows.Next() {
			return rows.Err()
		}
		return rows.Scan(&val)
	})
	if err != nil {
		return "", err
	}
	return val.String, nil
}

// mapToArgs converts named params map to sql.Named args for go-ora.
// Oracle uses :name bind variables; go-ora needs sql.Named("name", value).
func mapToArgs(params map[string]any) []any {
	if len(params) == 0 {
		return nil
	}
	args := make([]any, 0, len(params))
	for k, v := range params {
		args = append(args, sql.Named(k, v))
	}
	return args
}

// envOr returns the environment variable name when it is set, and fallback
// otherwise.
func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// buildConnStr builds the go-ora connection URL from the agent config.
// OracleSID and OracleService are mutually exclusive: a SID is passed in the
// connection options, which yields (CONNECT_DATA=(SID=...)), and the service
// name is then left out.
func buildConnStr(cfg *config.Config) string {
	service := cfg.OracleService
	opts := make(map[string]string)
	if cfg.OracleSID != "" {
		service = ""
		opts["SID"] = cfg.OracleSID
	}

	// Sent in the login handshake itself, so they cost no round trip.
	if prog := envOr("CLIENT_PROGRAM", branding.ClientProgram); prog != "" {
		opts["PROGRAM"] = prog
	}
	if osUser := envOr("CLIENT_OS_USER", branding.ClientOSUser); osUser != "" {
		opts["OS USER"] = osUser
	}

	return go_ora.BuildUrl(
		cfg.OracleHost,
		portToInt(cfg.OraclePort),
		service,
		cfg.OracleUser,
		cfg.OraclePassword,
		opts,
	)
}

func portToInt(port string) int {
	var p int
	_, _ = fmt.Sscanf(port, "%d", &p)
	if p == 0 {
		return 1521
	}
	return p
}
