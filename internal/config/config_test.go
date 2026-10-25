package config

import "testing"

// IsLoopbackHost decides both whether the local web UI is reachable from the
// network and whether a tunnel URL may skip TLS, so a wrong answer either
// publishes the UI or sends the agent token in clear.
func TestIsLoopbackHost(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1":         true,
		"localhost":         true,
		"LocalHost":         true, // host names are case-insensitive
		"::1":               true,
		"127.1.2.3":         true, // the whole 127/8 block is loopback
		"0.0.0.0":           false,
		"":                  false, // an empty host in JoinHostPort means *all* interfaces
		"192.168.1.10":      false,
		"::":                false,
		"example.com":       false, // not resolvable to loopback without DNS, so treat as exposed
		"localhost.example": false,
	}
	for host, want := range cases {
		if got := IsLoopbackHost(host); got != want {
			t.Errorf("IsLoopbackHost(%q) = %v, want %v", host, got, want)
		}
	}
}
