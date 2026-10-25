package proxy

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"reflect"
	"testing"
)

// answeringDB answers every query with the same rows. With fetchErr set, the
// fetch after the last row fails with it, as a query that times out or loses
// its session halfway through does.
type answeringDB struct {
	columns  []string
	rows     [][]driver.Value
	fetchErr error
	queries  int
}

func (d *answeringDB) Connect(context.Context) (driver.Conn, error) { return answeringSession{d}, nil }
func (d *answeringDB) Driver() driver.Driver                        { return nil }

type answeringSession struct{ db *answeringDB }

func (s answeringSession) Prepare(string) (driver.Stmt, error) { return answeringStmt(s), nil }
func (s answeringSession) Close() error                        { return nil }
func (s answeringSession) Begin() (driver.Tx, error)           { return nil, errors.New("not supported") }

type answeringStmt struct{ db *answeringDB }

func (s answeringStmt) Close() error  { return nil }
func (s answeringStmt) NumInput() int { return -1 }
func (s answeringStmt) Exec([]driver.Value) (driver.Result, error) {
	return nil, errors.New("not supported")
}
func (s answeringStmt) Query([]driver.Value) (driver.Rows, error) {
	s.db.queries++
	return &answeringRows{db: s.db}, nil
}

type answeringRows struct {
	db   *answeringDB
	next int
}

func (r *answeringRows) Columns() []string { return r.db.columns }
func (r *answeringRows) Close() error      { return nil }
func (r *answeringRows) Next(dest []driver.Value) error {
	if r.next < len(r.db.rows) {
		copy(dest, r.db.rows[r.next])
		r.next++
		return nil
	}
	if r.db.fetchErr != nil {
		return r.db.fetchErr
	}
	return io.EOF
}

// executorOn returns an executor whose queries d answers, with no session
// attributes to set first.
func executorOn(t *testing.T, d *answeringDB) *Executor {
	t.Helper()
	for _, name := range []string{"CLIENT_MODULE", "CLIENT_ACTION", "CLIENT_INFO"} {
		t.Setenv(name, "")
	}
	db := sql.OpenDB(d)
	t.Cleanup(func() { _ = db.Close() })
	return &Executor{db: db}
}

var errFetch = errors.New("ORA-01013: user requested cancel of current operation")

func TestExecuteReturnsEveryRowWithTextAsStrings(t *testing.T) {
	d := &answeringDB{
		columns: []string{"OWNER", "OBJECT_NAME", "LINE"},
		rows: [][]driver.Value{
			{[]byte("HR"), "EMPLOYEES", int64(1)},
			{[]byte("HR"), "DEPARTMENTS", nil},
		},
	}
	e := executorOn(t, d)

	got, err := e.Execute("SELECT owner, object_name, line FROM all_objects", nil)
	if err != nil {
		t.Fatalf("Execute = %v", err)
	}
	want := [][]any{{"HR", "EMPLOYEES", int64(1)}, {"HR", "DEPARTMENTS", nil}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Execute = %#v, want %#v", got, want)
	}
	if e.LastAnswer().IsZero() {
		t.Error("a query that completed is not recorded as an answer")
	}
}

func TestExecuteWithNoRowsReturnsAnEmptyList(t *testing.T) {
	e := executorOn(t, &answeringDB{columns: []string{"X"}})

	got, err := e.Execute("SELECT x FROM t", nil)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("Execute = %#v, %v, want an empty list, which serializes as [] rather than null", got, err)
	}
}

func TestExecuteOneReturnsTheFirstRow(t *testing.T) {
	e := executorOn(t, &answeringDB{
		columns: []string{"TEXT"},
		rows:    [][]driver.Value{{[]byte("first")}, {[]byte("second")}},
	})

	got, err := e.ExecuteOne("SELECT text FROM all_source", nil)
	if err != nil || !reflect.DeepEqual(got, []any{"first"}) {
		t.Fatalf("ExecuteOne = %#v, %v, want [first]", got, err)
	}
}

// No row is an answer: the platform reads it as "nothing found".
func TestExecuteOneWithNoRowIsAnAnswer(t *testing.T) {
	e := executorOn(t, &answeringDB{columns: []string{"X"}})

	got, err := e.ExecuteOne("SELECT x FROM t", nil)
	if err != nil || got != nil {
		t.Fatalf("ExecuteOne = %#v, %v, want no row and no error", got, err)
	}
	if e.LastAnswer().IsZero() {
		t.Error("a query that completed with no row is not recorded as an answer")
	}
}

// A fetch that fails before the first row is a failed query, not an empty
// result. Reported as no row, the platform would take an object it could not
// read for one that does not exist.
func TestExecuteOneReportsAFetchThatFails(t *testing.T) {
	e := executorOn(t, &answeringDB{columns: []string{"X"}, fetchErr: errFetch})

	got, err := e.ExecuteOne("SELECT x FROM t", nil)
	if !errors.Is(err, errFetch) || got != nil {
		t.Fatalf("ExecuteOne = %#v, %v, want the fetch error", got, err)
	}
	if !e.LastAnswer().IsZero() {
		t.Error("a query that failed is recorded as an answer")
	}
}

func TestExecuteReportsAFetchThatFailsHalfway(t *testing.T) {
	e := executorOn(t, &answeringDB{
		columns:  []string{"X"},
		rows:     [][]driver.Value{{"a"}},
		fetchErr: errFetch,
	})

	if _, err := e.Execute("SELECT x FROM t", nil); !errors.Is(err, errFetch) {
		t.Fatalf("Execute = %v, want the fetch error", err)
	}
	if !e.LastAnswer().IsZero() {
		t.Error("a query that failed is recorded as an answer")
	}
}

// The read-only check runs before a session is taken, in every mode.
func TestEveryModeRefusesAWriteBeforeReachingTheDatabase(t *testing.T) {
	d := &answeringDB{columns: []string{"X"}}
	e := executorOn(t, d)
	const write = "DELETE FROM t"

	if _, err := e.Execute(write, nil); !errors.Is(err, errNotAQuery) {
		t.Errorf("Execute(%q) = %v, want it refused", write, err)
	}
	if _, err := e.ExecuteOne(write, nil); !errors.Is(err, errNotAQuery) {
		t.Errorf("ExecuteOne(%q) = %v, want it refused", write, err)
	}
	if _, err := e.ExecuteClob(write, nil); !errors.Is(err, errNotAQuery) {
		t.Errorf("ExecuteClob(%q) = %v, want it refused", write, err)
	}
	if d.queries != 0 {
		t.Fatalf("%d statements reached the database, want 0", d.queries)
	}
}

func TestExecuteClobReturnsTheValueOrNothing(t *testing.T) {
	e := executorOn(t, &answeringDB{columns: []string{"DDL"}, rows: [][]driver.Value{{[]byte("CREATE TABLE t (x NUMBER)")}}})
	if got, err := e.ExecuteClob("SELECT ddl FROM t", nil); err != nil || got != "CREATE TABLE t (x NUMBER)" {
		t.Fatalf("ExecuteClob = %q, %v", got, err)
	}

	e = executorOn(t, &answeringDB{columns: []string{"DDL"}})
	if got, err := e.ExecuteClob("SELECT ddl FROM t", nil); err != nil || got != "" {
		t.Fatalf("ExecuteClob with no row = %q, %v, want an empty value", got, err)
	}

	e = executorOn(t, &answeringDB{columns: []string{"DDL"}, rows: [][]driver.Value{{nil}}})
	if got, err := e.ExecuteClob("SELECT ddl FROM t", nil); err != nil || got != "" {
		t.Fatalf("ExecuteClob of a NULL = %q, %v, want an empty value", got, err)
	}
}

// As for one row: a fetch that fails is a failed query, not a definition the
// object does not have.
func TestExecuteClobReportsAFetchThatFails(t *testing.T) {
	e := executorOn(t, &answeringDB{columns: []string{"DDL"}, fetchErr: errFetch})

	if got, err := e.ExecuteClob("SELECT ddl FROM t", nil); !errors.Is(err, errFetch) || got != "" {
		t.Fatalf("ExecuteClob = %q, %v, want the fetch error", got, err)
	}
	if !e.LastAnswer().IsZero() {
		t.Error("a query that failed is recorded as an answer")
	}
}
