package tunnel

import (
	"errors"
	"testing"
)

func TestValidateURL(t *testing.T) {
	cases := []struct {
		url string
		ok  bool
	}{
		{"wss://api.example.com/api/v1/tunnel/connect", true},
		{"WSS://api.example.com/api/v1/tunnel/connect", true}, // the scheme is case-insensitive
		{"  wss://api.example.com/api/v1/tunnel/connect  ", true},
		{"ws://localhost:8000/api/v1/tunnel/connect", true}, // a platform on this machine
		{"ws://LOCALHOST:8000/api/v1/tunnel/connect", true},
		{"ws://127.0.0.1:8000/api/v1/tunnel/connect", true},
		{"ws://[::1]:8000/api/v1/tunnel/connect", true},

		{"ws://api.example.com/api/v1/tunnel/connect", false}, // token and results in clear
		{"ws://192.168.1.20:8000/api/v1/tunnel/connect", false},
		{"ws://localhost.example.com/api/v1/tunnel/connect", false},
		{"ws://127.0.0.1.nip.io/api/v1/tunnel/connect", false},
		{"https://api.example.com/api/v1/tunnel/connect", false}, // the page's address, not the tunnel's
		{"http://localhost:8000/api/v1/tunnel/connect", false},
		{"api.example.com/api/v1/tunnel/connect", false},
		{"wss:///api/v1/tunnel/connect", false},
		{"", false},
	}
	for _, c := range cases {
		err := ValidateURL(c.url)
		if c.ok && err != nil {
			t.Errorf("ValidateURL(%q) = %v, want accepted", c.url, err)
		}
		if !c.ok && !errors.Is(err, ErrInvalidURL) {
			t.Errorf("ValidateURL(%q) = %v, want ErrInvalidURL", c.url, err)
		}
	}
}
