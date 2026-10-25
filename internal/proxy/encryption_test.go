package proxy

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/schemaxis-com/local-connector/internal/config"
)

// What Oracle Database Free 23.26.1 sends when its sqlnet.ora sets
// SQLNET.ENCRYPTION_SERVER=REQUIRED and SQLNET.ALLOW_WEAK_CRYPTO_CLIENTS=FALSE,
// recorded from a real one: the accept packet, then its answer to the agent's
// network encryption negotiation, which ends it with ORA-12269 before any
// login.
const (
	recordedAccept  = "0029000002000000013d08010000000001000000002951010000000000000000000100000020000000"
	recordedRefusal = "00000075062000000000deadbeef006b00000000000401000400030000000000040005171a100000020006" +
		"001f000e0001deadbeef00030000000200040001000100020000000000040005171a100000020006fbff00020001" +
		"00002fed00040005171a1000000300010000000000040005171a1000"
)

// refusingDatabase listens like that database and counts the connections made
// to it.
func refusingDatabase(t *testing.T) (host, port string, connections *atomic.Int32) {
	t.Helper()
	accept, err := hex.DecodeString(recordedAccept)
	if err != nil {
		t.Fatal(err)
	}
	refusal, err := hex.DecodeString(recordedRefusal)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	connections = new(atomic.Int32)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			go func() {
				defer func() { _ = c.Close() }()
				// Both answers go out at once: the driver reads each one only
				// after sending what it answers. Reading on keeps the socket
				// open until the driver has, since closing it with its bytes
				// unread would reset the connection.
				_, _ = c.Write(accept)
				_, _ = c.Write(refusal)
				_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
				_, _ = io.Copy(io.Discard, c)
			}()
		}
	}()
	host, port, err = net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return host, port, connections
}

// A database that refuses the agent's network encryption refuses it on every
// attempt, before the login, and writes each refusal to its alert log. The
// executor must say what was refused, in words, and not try again. The refusal
// goes through go-ora itself, so a driver upgrade that reports it differently
// fails here.
func TestARefusedEncryptionIsExplainedAndNotRetried(t *testing.T) {
	host, port, connections := refusingDatabase(t)
	e := newExecutor(&config.Config{
		OracleHost: host, OraclePort: port, OracleService: "FREEPDB1",
		OracleUser: "reader", OraclePassword: "pw",
	})
	t.Cleanup(func() { _ = e.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := e.Ping(ctx)
	var refused *EncryptionRefusedError
	if !errors.As(err, &refused) || refused.Code != 12269 {
		t.Fatalf("Ping = %v, want the ORA-12269 refusal", err)
	}
	if !strings.Contains(err.Error(), "SQLNET.ALLOW_WEAK_CRYPTO_CLIENTS=FALSE") {
		t.Errorf("Ping = %q, want it to name the setting that refused it", err)
	}
	if IsLoginRefused(err) {
		t.Error("a refused network encryption was reported as a refused login")
	}

	if err := e.Ping(ctx); !IsEncryptionRefused(err) {
		t.Errorf("second Ping = %v, want the same refusal", err)
	}
	if _, err := e.Execute("SELECT 1 FROM dual", nil); !IsEncryptionRefused(err) {
		t.Errorf("Execute = %v, want the same refusal", err)
	}
	if n := connections.Load(); n != 1 {
		t.Errorf("%d connections to the database, want 1", n)
	}
}

// Only the negotiation errors that repeat until the database's settings change
// are refusals. Any other error, including another negotiation error, is left
// as the driver reported it, and the executor keeps trying.
func TestOnlyNegotiationRefusalsAreExplained(t *testing.T) {
	for _, err := range []error{
		//nolint:misspell // go-ora's own spelling, reproduced verbatim
		errors.New("advanced negotiation error: during receive service header: network excpetion: ora-12599"),
		errors.New("dial tcp 127.0.0.1:1521: connect: connection refused"),
		errors.New("ORA-12514: TNS:listener does not currently know of service requested in connect descriptor"),
	} {
		if got := explainEncryptionRefusal(err); IsEncryptionRefused(got) || got.Error() != err.Error() {
			t.Errorf("explainEncryptionRefusal(%q) = %v, want it unchanged", err, got)
		}
	}
}
