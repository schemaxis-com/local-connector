package tunnel

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/schemaxis-com/local-connector/internal/config"
)

// ErrInvalidURL is wrapped by every error ValidateURL returns, so a caller can
// tell a tunnel URL that will never work from a connection that failed.
var ErrInvalidURL = errors.New("invalid tunnel URL")

// ValidateURL checks a tunnel URL before the agent stores it or dials it.
//
// The handshake carries the agent token and the tunnel then carries every
// query result, so the connection must be encrypted: wss://. Plain ws:// is
// accepted only when the host is this machine, a loopback name or address,
// which is how a developer reaches a platform running on http://localhost.
// Nothing leaves the machine there, so there is nothing to encrypt.
func ValidateURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Hostname() == "" {
		return fmt.Errorf("%w: it is not a valid address. Copy it from the Connections page, where it starts with wss://", ErrInvalidURL)
	}
	switch u.Scheme {
	case "wss":
		return nil
	case "ws":
		if config.IsLoopbackHost(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("%w: ws:// sends the agent token and every query result unencrypted, so it is accepted only for a platform running on this machine. Use the wss:// address shown on the Connections page", ErrInvalidURL)
	default:
		return fmt.Errorf("%w: it must start with wss://, as shown on the Connections page", ErrInvalidURL)
	}
}
