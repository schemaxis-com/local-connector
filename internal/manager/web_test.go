package manager

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The connection-manager UI has no authentication, and its handlers create,
// read, change and test the connection profiles, which hold the database
// credentials. The bind address is what keeps those handlers off the network
// the agent runs in. Listening on ":<port>" instead of "127.0.0.1:<port>" looks
// just as correct and offers the whole API to the network, which these tests
// would catch and nothing else would. How a host is classified as loopback is
// pinned in internal/config.

// startTestServer runs StartServer on host and returns the port it bound,
// cancelling the server when the test ends.
func startTestServer(t *testing.T, host string) int {
	t.Helper()
	return startTestServerWith(t, host, &ConnectionManager{})
}

// startTestServerWith is startTestServer for a test that needs a working
// manager behind the server.
func startTestServerWith(t *testing.T, host string, mgr *ConnectionManager) int {
	t.Helper()

	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Skipf("cannot bind %s on this machine: %v", host, err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go func() { _ = StartServer(ctx, host, port, mgr) }()

	// Wait for the listener to come up rather than sleeping a fixed amount.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(port)), 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return port
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("server on %s:%d never became reachable", host, port)
	return 0
}

func TestDefaultBindIsNotReachableFromANonLoopbackAddress(t *testing.T) {
	// The check itself: with the default host, a connection to the
	// machine's own routable address must be refused. If someone reverts the
	// bind to ":port" this dials successfully and the test fails.
	port := startTestServer(t, LoopbackHost)

	outbound := outboundIP(t)
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(outbound, fmt.Sprint(port)), 2*time.Second)
	if err == nil {
		_ = conn.Close()
		t.Fatalf(
			"the Web UI bound to %s accepted a connection on %s:%d, so it is "+
				"reachable from the network. The UI is unauthenticated and "+
				"manages database credentials; the default bind must stay "+
				"loopback-only.",
			LoopbackHost, outbound, port,
		)
	}
}

func TestLoopbackBindStillServesLocally(t *testing.T) {
	// The other half: locking the bind down must not break the normal case,
	// which is the whole reason the default is loopback rather than "off".
	port := startTestServer(t, LoopbackHost)

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port)) //nolint:noctx // short-lived test request
	if err != nil {
		t.Fatalf("loopback request failed, so the UI is unusable on the machine it runs on: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET / on loopback = %d, want 200", resp.StatusCode)
	}
}

func TestExplicitWideBindIsStillPossible(t *testing.T) {
	// The escape hatch must keep working: hardening the default is only
	// acceptable because the deliberate remote-admin case remains available.
	port := startTestServer(t, "0.0.0.0")

	outbound := outboundIP(t)
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(outbound, fmt.Sprint(port)), 2*time.Second)
	if err != nil {
		t.Fatalf("--bind 0.0.0.0 no longer exposes the UI (%v): the opt-in is broken", err)
	}
	_ = conn.Close()
}

// outboundIP returns a non-loopback address of this machine, skipping the test
// when there is none (an isolated CI container), since without one there is no
// way to distinguish a loopback bind from a wide one.
func outboundIP(t *testing.T) string {
	t.Helper()

	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skipf("cannot enumerate interfaces: %v", err)
	}
	for _, addr := range addrs {
		ipnet, ok := addr.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() || ipnet.IP.To4() == nil {
			continue
		}
		if !strings.HasPrefix(ipnet.IP.String(), "169.254.") { // skip link-local
			return ipnet.IP.String()
		}
	}
	t.Skip("no routable IPv4 address on this host: cannot test network reachability")
	return ""
}

// A loopback bind keeps the network out but not the user's own browser: any
// page they have open can send requests to 127.0.0.1. These cases pin what
// browserGuard refuses from such a page, and that the UI's own requests pass.
func TestBrowserGuard(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	cases := []struct {
		name    string
		bind    string
		method  string
		host    string
		headers map[string]string
		want    int
	}{
		{"UI page load", LoopbackHost, http.MethodGet, "localhost:8088", nil, http.StatusOK},
		{"UI page load by IP", LoopbackHost, http.MethodGet, "127.0.0.1:8088", nil, http.StatusOK},
		{"DNS rebinding read", LoopbackHost, http.MethodGet, "attacker.example:8088", nil, http.StatusForbidden},
		{"UI save", LoopbackHost, http.MethodPost, "localhost:8088",
			map[string]string{"Content-Type": "application/json", "Origin": "http://localhost:8088", "Sec-Fetch-Site": "same-origin"}, http.StatusOK},
		{"UI delete", LoopbackHost, http.MethodDelete, "localhost:8088",
			map[string]string{"Origin": "http://localhost:8088"}, http.StatusOK},
		{"cross-site simple POST", LoopbackHost, http.MethodPost, "localhost:8088",
			map[string]string{"Content-Type": "text/plain"}, http.StatusUnsupportedMediaType},
		{"foreign Origin", LoopbackHost, http.MethodPost, "localhost:8088",
			map[string]string{"Content-Type": "application/json", "Origin": "http://attacker.example"}, http.StatusForbidden},
		{"cross-site fetch metadata", LoopbackHost, http.MethodPost, "localhost:8088",
			map[string]string{"Content-Type": "application/json", "Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"wide bind reached by LAN address", "0.0.0.0", http.MethodGet, "192.168.1.10:8088", nil, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/api/connections", strings.NewReader("{}"))
			req.Host = tc.host
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			browserGuard(tc.bind, ok).ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

// A plaintext tunnel URL to another host is refused when the profile is saved,
// not only when its tunnel starts, so the user sees why at once. Without the
// check the save succeeds and the profile is stored.
func TestSaveRefusesPlaintextRemoteTunnelURL(t *testing.T) {
	dir := t.TempDir()
	mgr, err := NewConnectionManager(filepath.Join(dir, "connections.json"), filepath.Join(dir, ".agent.key"))
	if err != nil {
		t.Fatalf("NewConnectionManager: %v", err)
	}
	port := startTestServerWith(t, LoopbackHost, mgr)

	body := `{"id":"conn_1","label":"prod","oracle_host":"db.internal","oracle_port":"1521",` +
		`"oracle_service":"ORCL","oracle_user":"app","oracle_password":"pw",` +
		`"tunnel_url":"ws://platform.example.com/api/v1/tunnel/connect","agent_token":"tok","enabled":false}`
	resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/api/connections", port), "application/json", strings.NewReader(body)) //nolint:noctx // short-lived test request
	if err != nil {
		t.Fatalf("save request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("save with a remote ws:// URL = %d, want 400", resp.StatusCode)
	}
	if n := len(mgr.GetProfiles()); n != 0 {
		t.Errorf("%d profile(s) stored, want none", n)
	}
}

// A tunnel URL copied by hand often brings a space with it. The check accepts
// it with the space around it, and the dialer does not, so the profile stores
// the URL as the tunnel will dial it.
func TestSaveTrimsTheTunnelURL(t *testing.T) {
	mgr := newTestManager(t)
	port := startTestServerWith(t, LoopbackHost, mgr)

	body := `{"id":"conn_1","label":"prod","oracle_host":"db.internal","oracle_port":"1521",` +
		`"oracle_service":"ORCL","oracle_user":"app","oracle_password":"pw",` +
		`"tunnel_url":"  wss://platform.example.com/api/v1/tunnel/connect \t","agent_token":"tok","enabled":false}`
	resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/api/connections", port), "application/json", strings.NewReader(body)) //nolint:noctx // short-lived test request
	if err != nil {
		t.Fatalf("save request failed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("save = %d, want 200", resp.StatusCode)
	}

	profiles := mgr.GetProfiles()
	if len(profiles) != 1 {
		t.Fatalf("%d profile(s) stored, want 1", len(profiles))
	}
	if got, want := profiles[0].TunnelURL, "wss://platform.example.com/api/v1/tunnel/connect"; got != want {
		t.Errorf("stored tunnel URL = %q, want %q", got, want)
	}
}

// The UI writes a profile ID into URL paths and inline click handlers, so an
// ID is the one profile field that cannot carry arbitrary text.
func TestProfileIDPattern(t *testing.T) {
	for _, id := range []string{"conn_1727460000000", "prod-db", "A1"} {
		if !profileIDPattern.MatchString(id) {
			t.Errorf("%q refused, want accepted", id)
		}
	}
	for _, id := range []string{"", "x');alert(1);//", "a/b", "a b", strings.Repeat("a", 65)} {
		if profileIDPattern.MatchString(id) {
			t.Errorf("%q accepted, want refused", id)
		}
	}
}

// The UI acts on the IDs it listed, so an unknown one is a stale page or a
// typo: the answer says it does not exist, not that the agent failed. An
// empty manager lists its connections as [], not null.
func TestUnknownProfilesAndEmptyLists(t *testing.T) {
	port := startTestServerWith(t, LoopbackHost, newTestManager(t))
	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	for _, path := range []string{"/api/connections", "/api/profiles"} {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if got := strings.TrimSpace(string(body)); got != "[]" {
			t.Errorf("GET %s on an empty manager = %s, want []", path, got)
		}
	}

	del, _ := http.NewRequest(http.MethodDelete, base+"/api/connections/conn_unknown", nil)
	toggle, _ := http.NewRequest(http.MethodPost, base+"/api/connections/conn_unknown/toggle", strings.NewReader(`{"enabled":true}`))
	toggle.Header.Set("Content-Type", "application/json")
	for _, req := range []*http.Request{del, toggle} {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404", req.Method, req.URL.Path, resp.StatusCode)
		}
	}
}
