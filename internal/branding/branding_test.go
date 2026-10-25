package branding

import "testing"

// TestLogSQLShipsOff pins the compile-time default of the SQL-text trace.
//
// Logged by default, the query text would end up in every screenshot, support
// ticket and log bundle that leaves the machine (see LogSQL). The flip is one
// character wide and the kind of thing a debugging session leaves behind, so
// the default is pinned rather than only commented. Tracing for one run is
// what AGENT_LOG_SQL is for. Like LogSQL itself, this guards log hygiene, not
// security: the database shows the same queries in V$SQL.
func TestLogSQLShipsOff(t *testing.T) {
	if LogSQL {
		t.Fatal("LogSQL must ship false: the platform's SQL text would land in local logs by default")
	}
}

// TestAppNameIsOverridableAtLinkTime keeps AppName a variable. The linker's
// -X flag sets only variables and says nothing when its target is a constant,
// so turning AppName back into a const would make every build that overrides
// it quietly ship the default name instead. Taking its address does not
// compile for a constant, which is what this test relies on.
func TestAppNameIsOverridableAtLinkTime(t *testing.T) {
	if name := &AppName; *name == "" {
		t.Fatal("AppName is empty: the UI title, logs and data directory need a name")
	}
}

// A build carrying the default name is the product LegacyAppNames describes;
// any other name set at link time is not, and must not migrate its data.
func TestAppNameOverridden(t *testing.T) {
	saved := AppName
	t.Cleanup(func() { AppName = saved })

	AppName = defaultAppName
	if AppNameOverridden() {
		t.Error("the default name is reported as an override")
	}
	AppName = "InternalBuild"
	if !AppNameOverridden() {
		t.Error("a name set at link time is not reported as an override")
	}
}

// LegacyDirNames builds the Linux and macOS directory names, which are the
// lower-case form of the product name, as DirName does for the current one.
func TestLegacyDirNamesAreLowerCase(t *testing.T) {
	saved := LegacyAppNames
	t.Cleanup(func() { LegacyAppNames = saved })

	LegacyAppNames = []string{"OldName", "Older"}
	got := LegacyDirNames()
	if len(got) != 2 || got[0] != "oldname" || got[1] != "older" {
		t.Errorf("LegacyDirNames() = %v, want [oldname older]", got)
	}
}
