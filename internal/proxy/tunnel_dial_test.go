package proxy

// Tests for dialTunnel's failure classification (issue #219): a client
// hanging up mid-dial must log at Debug, everything else (our own deadline,
// connection refused, unresolvable host) must stay at Warn — and the
// elapsed dial time must appear on the line either way.
//
// dial is a fake dialFunc throughout: no test here makes a real network
// call, so nothing in this file is affected by the live HTTPS_PROXY in the
// shell env or by DNS.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"
)

// newDialLogger builds a logger over a syncBuffer (defined in
// representative_claim_test.go) at Debug level, so both WARN and DEBUG
// lines land in the buffer for the assertions below to tell apart.
func newDialLogger() (*slog.Logger, *syncBuffer) {
	buf := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return logger, buf
}

func TestDialTunnelClassification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// dial simulates the egress dialer. It receives the per-dial
		// context (parent + tunnelDialTimeout) and the cancel func for the
		// PARENT context, so it can simulate the client hanging up
		// mid-dial.
		dial      func(cancelParent context.CancelFunc) dialFunc
		timeout   time.Duration
		wantLevel string // "DEBUG" or "WARN"
	}{
		{
			name: "client cancelled mid-dial",
			dial: func(cancelParent context.CancelFunc) dialFunc {
				return func(ctx context.Context, network, addr string) (net.Conn, error) {
					// The client hangs up right as the dial starts (e.g. an
					// MCP teardown aborting a DNS lookup) — long before our
					// own 10s deadline would ever fire.
					cancelParent()
					<-ctx.Done()
					return nil, ctx.Err()
				}
			},
			timeout:   10 * time.Second,
			wantLevel: "DEBUG",
		},
		{
			name: "our own deadline expires",
			dial: func(context.CancelFunc) dialFunc {
				return func(ctx context.Context, network, addr string) (net.Conn, error) {
					<-ctx.Done()
					// What a real deadline-expired dial actually returns:
					// a net.Error whose message is "i/o timeout", NOT
					// context.DeadlineExceeded.
					return nil, &net.OpError{Op: "dial", Net: "tcp", Err: fmt.Errorf("i/o timeout")}
				}
			},
			timeout:   20 * time.Millisecond,
			wantLevel: "WARN",
		},
		{
			name: "connection refused",
			dial: func(context.CancelFunc) dialFunc {
				return func(ctx context.Context, network, addr string) (net.Conn, error) {
					return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}
				}
			},
			timeout:   10 * time.Second,
			wantLevel: "WARN",
		},
		{
			name: "unresolvable host",
			dial: func(context.CancelFunc) dialFunc {
				return func(ctx context.Context, network, addr string) (net.Conn, error) {
					return nil, &net.DNSError{Err: "no such host", Name: addr, IsNotFound: true}
				}
			},
			timeout:   10 * time.Second,
			wantLevel: "WARN",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logger, buf := newDialLogger()
			parent, cancelParent := context.WithCancel(context.Background())
			defer cancelParent()

			_, err := dialTunnel(parent, logger, tc.dial(cancelParent), "example.invalid", "443", tc.timeout)
			if err == nil {
				t.Fatal("dialTunnel: want error, got nil")
			}

			out := buf.String()
			line := findLine(out, "tunnel dial failed")
			if line == "" {
				t.Fatalf("no \"tunnel dial failed\" line in log output:\n%s", out)
			}
			if !strings.Contains(line, "level="+tc.wantLevel) {
				t.Errorf("wrong level: want %s, got line:\n%s", tc.wantLevel, line)
			}
			if !strings.Contains(line, "elapsed=") {
				t.Errorf("elapsed dial time missing from log line:\n%s", line)
			}
			if !strings.Contains(line, "host=example.invalid") {
				t.Errorf("host missing from log line:\n%s", line)
			}
		})
	}
}

// TestDialTunnelSuccessSkipsLogging confirms a successful dial (no error)
// never touches the logger, so the classification above only fires on the
// failure path.
func TestDialTunnelSuccessSkipsLogging(t *testing.T) {
	t.Parallel()
	logger, buf := newDialLogger()

	client, srv := net.Pipe()
	defer srv.Close()
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return client, nil
	}

	up, err := dialTunnel(context.Background(), logger, dial, "example.invalid", "443", time.Second)
	if err != nil {
		t.Fatalf("dialTunnel: unexpected error: %v", err)
	}
	defer up.Close()

	if out := buf.String(); out != "" {
		t.Errorf("expected no log output on success, got:\n%s", out)
	}
}

// findLine returns the first line in out containing substr, or "".
func findLine(out, substr string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, substr) {
			return line
		}
	}
	return ""
}
