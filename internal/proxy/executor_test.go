package proxy

import (
	"net/url"
	"testing"

	"github.com/schemaxis-com/local-connector/internal/config"
	go_ora "github.com/sijms/go-ora/v2"
)

// go-ora's driver stores the character set of the first database it connects
// to and decodes every later connection it opens with it. One agent serves
// several databases, so two executors sharing a driver would read the second
// database's text with the first one's character set. Each must have its own,
// and neither may be the driver go-ora registers for the whole process.
func TestEachExecutorHasADriverOfItsOwn(t *testing.T) {
	first := newExecutor(&config.Config{
		OracleHost: "db1.internal", OraclePort: "1521",
		OracleService: "SALES", OracleUser: "app", OraclePassword: "pw",
	})
	t.Cleanup(func() { _ = first.Close() })
	second := newExecutor(&config.Config{
		OracleHost: "db2.internal", OraclePort: "1521",
		OracleService: "BILLING", OracleUser: "app", OraclePassword: "pw",
	})
	t.Cleanup(func() { _ = second.Close() })

	if first.db.Driver() == second.db.Driver() {
		t.Fatal("two executors share one driver, so the second database is decoded with the first one's character set")
	}
	for _, e := range []*Executor{first, second} {
		if e.db.Driver() == go_ora.GetDefaultDriver() {
			t.Fatal("an executor uses the driver go-ora shares across the process")
		}
	}
}

func TestBuildConnStr(t *testing.T) {
	t.Setenv("CLIENT_PROGRAM", "")
	t.Setenv("CLIENT_OS_USER", "")

	tests := []struct {
		name string
		cfg  *config.Config
		want string
	}{
		{
			name: "service name",
			cfg: &config.Config{
				OracleHost: "db.internal", OraclePort: "1521",
				OracleService: "ORCLPDB1", OracleUser: "app", OraclePassword: "pw",
			},
			want: "oracle://app:pw@db.internal:1521/ORCLPDB1?PROGRAM=SchemAxis+Agent",
		},
		{
			name: "sid takes precedence and omits service",
			cfg: &config.Config{
				OracleHost: "db.internal", OraclePort: "1521",
				OracleSID: "XE", OracleUser: "app", OraclePassword: "pw",
			},
			want: "oracle://app:pw@db.internal:1521/?SID=XE&PROGRAM=SchemAxis+Agent",
		},
		{
			name: "empty port falls back to 1521",
			cfg: &config.Config{
				OracleHost: "db.internal", OraclePort: "",
				OracleService: "ORCL", OracleUser: "app", OraclePassword: "pw",
			},
			want: "oracle://app:pw@db.internal:1521/ORCL?PROGRAM=SchemAxis+Agent",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildConnStr(tt.cfg)
			gotURL, err := url.Parse(got)
			if err != nil {
				t.Fatalf("failed to parse connection URL %q: %v", got, err)
			}
			wantURL, err := url.Parse(tt.want)
			if err != nil {
				t.Fatalf("failed to parse expected URL %q: %v", tt.want, err)
			}

			if gotURL.Scheme != wantURL.Scheme || gotURL.Host != wantURL.Host || gotURL.Path != wantURL.Path {
				t.Fatalf("buildConnStr() = %q, want components like %q", got, tt.want)
			}
			if gotURL.User.String() != wantURL.User.String() {
				t.Fatalf("buildConnStr() user = %q, want %q", gotURL.User.String(), wantURL.User.String())
			}

			gotQuery := gotURL.Query()
			wantQuery := wantURL.Query()
			if len(gotQuery) != len(wantQuery) {
				t.Fatalf("buildConnStr() query length = %d (%v), want %d (%v)", len(gotQuery), gotQuery, len(wantQuery), wantQuery)
			}
			for k, v := range wantQuery {
				if gotQuery.Get(k) != v[0] {
					t.Fatalf("buildConnStr() query param %q = %q, want %q", k, gotQuery.Get(k), v[0])
				}
			}
		})
	}
}
