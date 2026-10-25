// Package config holds the resolved settings for a single agent connection,
// plus a small .env loader for process-wide overrides read at startup.
package config

import (
	"bufio"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
)

// Config holds the settings of one connection: the Oracle database it opens
// and the tunnel that serves it.
type Config struct {
	// Oracle connection settings. A service name or a SID identifies the
	// database, as in SQL Developer's "Service Name vs SID" Basic-connection
	// choice. When both are set, the SID is used and the service name ignored.
	OracleHost     string
	OraclePort     string
	OracleService  string
	OracleSID      string
	OracleUser     string
	OraclePassword string

	// Tunnel settings
	TunnelURL  string
	AgentToken string
}

// Target returns a human-readable description of the Oracle target for logs,
// using classic Oracle notation: host:port/service for a service name, or
// host:port:SID for a SID.
func (c *Config) Target() string {
	if c.OracleSID != "" {
		return fmt.Sprintf("%s:%s:%s", c.OracleHost, c.OraclePort, c.OracleSID)
	}
	return fmt.Sprintf("%s:%s/%s", c.OracleHost, c.OraclePort, c.OracleService)
}

// IsLoopbackHost reports whether host names this machine: "localhost" or a
// loopback address. It classifies both the address the local web UI binds to
// and the host of a tunnel URL, so the two decisions cannot drift apart.
func IsLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// LoadDotEnv parses a .env file and sets variables that are not already set in the environment.
// Lines starting with # are treated as comments. Inline comments are not supported.
// Quotes around values are stripped.
func LoadDotEnv(path string) {
	f, err := os.Open(path) //nolint:gosec // path is a known configuration file path, not untrusted input
	if err != nil {
		return // file missing is fine
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		// Strip surrounding quotes
		if len(val) >= 2 && ((val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'')) {
			val = val[1 : len(val)-1]
		}
		// Only set if not already set in environment
		if os.Getenv(key) == "" {
			_ = os.Setenv(key, val)
		}
	}

	if err := scanner.Err(); err != nil {
		log.Printf("Error scanning .env file: %v", err)
	}
}
