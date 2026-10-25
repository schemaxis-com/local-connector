// Package branding holds the product name shown in the agent's console
// output and web UI and used for its local data directory, the build version,
// and the values the agent reports to Oracle about its sessions.
//
// A rename changes defaultAppName and appends the previous value to
// LegacyAppNames, so that installed agents move their data directory to the
// new name. The binary name, db-agent, is not derived from the product name.
package branding

import "strings"

// defaultAppName is the name this source tree ships under. A rename changes
// it here, never through -X: LegacyAppNames records the earlier values of this
// constant, so only a build that carries it migrates a data directory.
const defaultAppName = "SchemAxis"

// AppName is the product name shown in the agent's console output, embedded
// web UI and handshake User-Agent, and used to derive the local data directory
// name. A var rather than a const so a build can set it with
// -ldflags "-X <module>/internal/branding.AppName=...", where <module> is the
// module path declared in go.mod.
//
// Changing it moves the data directory. A rename carries the saved
// connections over only if the previous name is listed in LegacyAppNames, and
// a build that sets it with -X never migrates (see AppNameOverridden).
var AppName = defaultAppName

// Version is the agent build version, injected at build time via
// -ldflags "-X <module>/internal/branding.Version=...". "dev" is the fallback
// for local, non-release builds. It lives here (not in package main) so
// every layer that reports the version (the --version flag, startup log,
// and the tunnel's X-Agent-Version handshake header) reads one symbol.
var Version = "dev"

// LegacyAppNames lists every previous value of defaultAppName, oldest first:
// a rename appends to it. On startup, DataDir() looks for their data
// directories from the most recent name back and moves the first one it finds
// to the current AppName location, so upgrading users keep their
// connections.json and .agent.key across rebrands. Empty while no release
// under another name has been distributed.
//
// Format, after two renames (FirstName shipped, then SecondName, and
// defaultAppName is now the third):
//
//	var LegacyAppNames = []string{"FirstName", "SecondName"}
//
// List only names a release has actually shipped under.
var LegacyAppNames = []string{}

// AppNameOverridden reports whether this build set AppName at link time, for
// instance a test build under another name. Such a build keeps a
// data directory of its own and never migrates one: LegacyAppNames lists the
// earlier names of the default product, not of the override, so migrating
// would move a normal installation's saved connections into the test build.
func AppNameOverridden() bool {
	return AppName != defaultAppName
}

// DirName returns the lowercase, filesystem-safe form of AppName used for
// the local data directory on Linux and macOS ("~/.<dirname>/").
func DirName() string {
	return strings.ToLower(AppName)
}

// LegacyDirNames returns the lowercase form of each entry in LegacyAppNames,
// used to locate a pre-rebrand data directory for one-time migration.
func LegacyDirNames() []string {
	names := make([]string, len(LegacyAppNames))
	for i, n := range LegacyAppNames {
		names[i] = strings.ToLower(n)
	}
	return names
}

// ============================================================================
// Session identification (V$SESSION attributes)
// ============================================================================
//
// The values below are what a DBA sees in V$SESSION for the agent's
// connections. They exist so the operator can make those sessions easy to
// find, filter and audit in their own monitoring: match a naming convention,
// scope a Resource Manager plan, or read them in an audit trail. INSTALL.md
// documents the environment variables.
//
// Each field has a built-in default here and an optional per-run override
// through the matching CLIENT_* environment variable, read each time the
// agent opens or uses a database connection (see internal/proxy/executor.go).
// A deployment that wants different values baked into the binary can also set
// them at build time with
// -ldflags "-X '<module>/internal/branding.ClientProgram=...'". The defaults
// below identify the connection clearly as this agent.

// ClientProgram is the program name reported to the database
// (V$SESSION.PROGRAM), evaluated during the initial TNS handshake with no
// extra round trip. Defaults to a clear identifier for the agent so the
// connection is unambiguous in DBA connection logs. Override with
// CLIENT_PROGRAM.
var ClientProgram = AppName + " Agent"

// ClientOSUser is the OS username reported to the database
// (V$SESSION.OSUSER), evaluated during the initial TNS handshake with no
// extra round trip. Empty by default, which lets the go-ora driver report
// the actual OS user running the agent process. Override with
// CLIENT_OS_USER.
var ClientOSUser = ""

// ClientModule is the application module (V$SESSION.MODULE), set via PL/SQL
// each time a query takes a connection from the pool. Setting it adds one
// small round trip per query, so it defaults to empty (disabled).
// Override with CLIENT_MODULE, e.g. AppName + " Query Executor" to classify
// the agent's sessions in DBA views.
var ClientModule = ""

// ClientAction is the active operation name (V$SESSION.ACTION), set via
// PL/SQL each time a query takes a connection from the pool. Same small
// per-query cost as ClientModule, so it defaults to empty (disabled).
// Override with CLIENT_ACTION.
var ClientAction = ""

// ClientInfo is the custom client info string (V$SESSION.CLIENT_INFO), set
// via PL/SQL each time a query takes a connection from the pool. Same small
// per-query cost as ClientModule, so it defaults to empty (disabled).
// Override with CLIENT_INFO.
var ClientInfo = ""

// ============================================================================
// Diagnostics
// ============================================================================

// LogSQL controls whether the SQL text of each query is written to the agent
// log. It ships false: the queries are the platform's own work, and a local
// log ends up in screenshots, support tickets and log bundles that leave the
// machine it was written on.
//
// This is log hygiene, not a security boundary. The queries run against the
// customer's own database, so anyone with access to it can read them from
// V$SQL, V$SQLTEXT, AWR or a session trace without touching this binary. That
// is why the AGENT_LOG_SQL environment variable may turn it on for a support
// session (see internal/tunnel/client.go) instead of requiring a special build.
var LogSQL = false
