package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"mime"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/schemaxis-com/local-connector/internal/branding"
	"github.com/schemaxis-com/local-connector/internal/config"
	"github.com/schemaxis-com/local-connector/internal/tunnel"
)

// LoopbackHost is the default bind address for the connection-manager UI.
//
// The UI has no authentication, and its handlers create, read, change and test
// connection profiles, which hold the database credentials. Bound to every
// interface, it would offer them to the whole network the agent runs in.
// Loopback keeps the normal case (a browser on this machine) working and makes
// exposing the UI to the network an explicit choice, which StartServer logs.
const LoopbackHost = "127.0.0.1"

// profileIDPattern bounds a connection profile ID to characters that are safe
// wherever the UI puts an ID: a URL path segment and an inline handler.
var profileIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// browserGuard refuses the requests a web page the user happens to have open
// could make to this server. Binding to loopback keeps the network out, but not
// the user's own browser, which can reach 127.0.0.1 from any site:
//
//   - A cross-site POST with a text/plain body needs no CORS preflight, so
//     without a check any page could create or rewrite a connection profile.
//     A state-changing request must therefore be JSON (which a cross-site page
//     cannot send without a preflight this server never approves), and must
//     not declare a foreign Origin or a cross-site Sec-Fetch-Site.
//   - DNS rebinding points an attacker's hostname at 127.0.0.1, which turns
//     every read into a same-origin read. On a loopback bind the Host header
//     must therefore name a loopback host.
//
// On a wide bind (--bind 0.0.0.0) the host names the UI is reached by are not
// known here, so the Host check is skipped there and only the firewall the
// startup warning asks for stands in front of it.
func browserGuard(bindHost string, next http.Handler) http.Handler {
	checkHost := config.IsLoopbackHost(bindHost)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if checkHost && !config.IsLoopbackHost(hostOnly(r.Host)) {
			writeJSONError(w, http.StatusForbidden, "This address does not serve the connection manager. Open it at http://localhost instead.")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
				writeJSONError(w, http.StatusForbidden, "Requests from another site are refused.")
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+r.Host {
				writeJSONError(w, http.StatusForbidden, "Requests from another site are refused.")
				return
			}
			if r.Method != http.MethodDelete {
				if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "application/json" {
					writeJSONError(w, http.StatusUnsupportedMediaType, "The request body must be JSON.")
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// hostOnly strips the port from a Host header value, if it has one.
func hostOnly(hostport string) string {
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		return host
	}
	return strings.Trim(hostport, "[]")
}

// StartServer starts the embedded web server on host:port. A host other than
// loopback exposes an unauthenticated API that manages credentials to the
// network, so it is logged as a warning.
func StartServer(ctx context.Context, host string, port int, mgr *ConnectionManager) error {
	mux := http.NewServeMux()

	// The connection manager's page.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		tmpl, err := template.ParseFS(WebAssets, "web/index.html")
		if err != nil {
			http.Error(w, "Embedded asset not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = tmpl.Execute(w, map[string]string{"AppName": branding.AppName})
	})

	// GET /api/connections: the status of every connection.
	mux.HandleFunc("/api/connections", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(mgr.GetConnections())
			return
		}

		// POST /api/connections: create or update a profile.
		if r.Method == http.MethodPost {
			var p ConnectionProfile
			if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
				writeJSONError(w, http.StatusBadRequest, "Invalid request body")
				return
			}

			// Stored as the tunnel will dial it. A URL copied by hand often
			// brings a space along, which ValidateURL ignores and the dialer
			// does not.
			p.TunnelURL = strings.TrimSpace(p.TunnelURL)

			if !profileIDPattern.MatchString(p.ID) {
				writeJSONError(w, http.StatusBadRequest, "The profile ID may contain only letters, digits, '_' and '-'")
				return
			}
			// A service name or a SID identifies the database. The UI sends
			// one of them; if both arrive, internal/proxy connects by the SID.
			if p.Label == "" || p.OracleHost == "" || (p.OracleService == "" && p.OracleSID == "") || p.OracleUser == "" || p.OraclePassword == "" || p.TunnelURL == "" || p.AgentToken == "" {
				writeJSONError(w, http.StatusBadRequest, "Missing required configuration fields")
				return
			}
			if err := tunnel.ValidateURL(p.TunnelURL); err != nil {
				writeJSONError(w, http.StatusBadRequest, err.Error())
				return
			}

			// SaveConnection encrypts the password and the token.
			if err := mgr.SaveConnection(&p); err != nil {
				writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to save profile: %v", err))
				return
			}

			if p.Enabled {
				mgr.startTunnel(ctx, p.ID)
			}

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
			return
		}

		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	})

	// GET /api/profiles: the profiles, with the password and token masked.
	mux.HandleFunc("/api/profiles", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mgr.GetProfiles())
	})

	// POST /api/connections/test: try to log in to a profile's database.
	mux.HandleFunc("/api/connections/test", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var p ConnectionProfile
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			writeJSONError(w, http.StatusBadRequest, "Invalid request body")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		if err := mgr.TestConnection(&p); err != nil {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": false,
				"error":   err.Error(),
			})
			return
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
		})
	})

	// GET /api/logs: the most recent log lines.
	mux.HandleFunc("/api/logs", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mgr.GetLogs())
	})

	// POST /api/connections/{id}/toggle and DELETE /api/connections/{id}.
	mux.HandleFunc("/api/connections/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		if r.Method == http.MethodPost && strings.HasSuffix(path, "/toggle") {
			id := strings.TrimPrefix(path, "/api/connections/")
			id = strings.TrimSuffix(id, "/toggle")

			var body struct {
				Enabled bool `json:"enabled"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeJSONError(w, http.StatusBadRequest, "Invalid request body")
				return
			}

			if err := mgr.ToggleConnection(ctx, id, body.Enabled); err != nil {
				writeJSONError(w, statusFor(err), err.Error())
				return
			}

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
			return
		}

		if r.Method == http.MethodDelete {
			id := strings.TrimPrefix(path, "/api/connections/")
			if id == "" {
				writeJSONError(w, http.StatusBadRequest, "Missing profile ID")
				return
			}

			if err := mgr.DeleteConnection(id); err != nil {
				writeJSONError(w, statusFor(err), err.Error())
				return
			}

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]bool{"success": true})
			return
		}

		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	})

	serverAddr := net.JoinHostPort(host, strconv.Itoa(port))
	mgr.AddLog(fmt.Sprintf("Web UI Server starting on http://%s", serverAddr))
	if !config.IsLoopbackHost(host) {
		mgr.AddLog(fmt.Sprintf(
			"WARNING: the Web UI is bound to %s, so it is reachable from the "+
				"network. It has no authentication: anyone who can reach this "+
				"port can read and modify connection profiles. Restrict it with "+
				"a firewall rule, or drop --bind to return to loopback-only.",
			host,
		))
	}

	server := &http.Server{
		Addr:              serverAddr,
		Handler:           browserGuard(host, mux),
		ReadHeaderTimeout: 3 * time.Second,
	}

	// Cancelling ctx shuts the server down, letting requests in flight finish.
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}

// statusFor is the HTTP status for an error from the manager: 404 for a
// profile it does not hold, 500 for anything else.
func statusFor(err error) int {
	if errors.Is(err, ErrProfileNotFound) {
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}

func writeJSONError(w http.ResponseWriter, statusCode int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
