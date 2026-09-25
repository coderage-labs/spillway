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
	"sync/atomic"
	"testing"
	"time"
)

// fakeTimeout implements net.Error the way a real dial timeout does: its
// message is "i/o timeout" and Timeout() genuinely reports true. Go's real
// timeout sentinel (internal/poll's timeoutError) has exactly this shape,
// unlike the plain fmt.Errorf stand-ins used above for #219's classification
// cases, which deliberately don't implement Timeout() because that test
// only cares about client-gone-vs-not, not about retryability.
type fakeTimeout struct{}

func (fakeTimeout) Error() string   { return "i/o timeout" }
func (fakeTimeout) Timeout() bool   { return true }
func (fakeTimeout) Temporary() bool { return true }

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

			_, err := dialTunnel(parent, logger, tc.dial(cancelParent), "example.invalid", "443", tc.timeout, time.Millisecond)
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

	up, err := dialTunnel(context.Background(), logger, dial, "example.invalid", "443", time.Second, time.Millisecond)
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

// timeoutErr wraps fakeTimeout the way a real dial does: net.OpError.
// Timeout() only reports true when the wrapped Err itself implements the
// timeout interface, which is exactly what fakeTimeout is for.
func timeoutErr() error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: fakeTimeout{}}
}

// TestDialTunnelRetriesTransientFailure covers issue #221's core case: a
// dial that fails once with a retryable error and succeeds on the very next
// attempt — the api.github.com pattern from the live log, which failed 44
// times and worked on the immediately following attempt every time. One
// retry, tunnel established, and nothing about it should reach WARN: the
// operator did not have a real failure to look at.
func TestDialTunnelRetriesTransientFailure(t *testing.T) {
	t.Parallel()
	logger, buf := newDialLogger()

	client, srv := net.Pipe()
	defer srv.Close()

	var attempts atomic.Int32
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		n := attempts.Add(1)
		if n == 1 {
			return nil, timeoutErr()
		}
		return client, nil
	}

	up, err := dialTunnel(context.Background(), logger, dial, "example.invalid", "443", time.Second, time.Millisecond)
	if err != nil {
		t.Fatalf("dialTunnel: unexpected error: %v", err)
	}
	defer up.Close()

	if got := attempts.Load(); got != 2 {
		t.Errorf("attempts = %d, want 2 (one failure, one retry)", got)
	}
	if out := buf.String(); strings.Contains(out, "level=WARN") {
		t.Errorf("expected no WARN for a retry that succeeded, got:\n%s", out)
	}
}

// TestDialTunnelExhaustsRetryAfterTwoFailures covers a dial that fails
// twice: the retry buys nothing, the caller gets its 502 (via the returned
// error), and the failure reaches WARN exactly once, at tunnelDialMaxAttempts
// attempts — not zero, and not more.
func TestDialTunnelExhaustsRetryAfterTwoFailures(t *testing.T) {
	t.Parallel()
	logger, buf := newDialLogger()

	var attempts atomic.Int32
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		attempts.Add(1)
		return nil, timeoutErr()
	}

	_, err := dialTunnel(context.Background(), logger, dial, "example.invalid", "443", time.Second, time.Millisecond)
	if err == nil {
		t.Fatal("dialTunnel: want error, got nil")
	}

	if got := attempts.Load(); got != tunnelDialMaxAttempts {
		t.Errorf("attempts = %d, want %d", got, tunnelDialMaxAttempts)
	}
	out := buf.String()
	line := findLine(out, "tunnel dial failed")
	if line == "" {
		t.Fatalf("no \"tunnel dial failed\" line in log output:\n%s", out)
	}
	if !strings.Contains(line, "level=WARN") {
		t.Errorf("wrong level: want WARN, got line:\n%s", line)
	}
	if !strings.Contains(line, fmt.Sprintf("attempt=%d", tunnelDialMaxAttempts)) {
		t.Errorf("wrong attempt count on WARN line: want attempt=%d, got:\n%s", tunnelDialMaxAttempts, line)
	}
	if n := strings.Count(out, "tunnel dial failed"); n != 1 {
		t.Errorf("expected exactly one \"tunnel dial failed\" log line, got %d:\n%s", n, out)
	}
}

// TestDialTunnelNoRetryWhenClientCancelled covers the constraint from issue
// #221 that a client already gone must never be retried — nobody is
// waiting for the result. Asserts the attempt count directly, not just the
// log level, since a retry could in principle happen without ever changing
// which level the final line lands at.
//
// The first attempt fails with an ordinary transient (retryable) error
// while the client is still there, so dialTunnel commits to waiting out
// the backoff before trying again — exactly the MCP-teardown case from
// issue #219, except this time the client hangs up *during* that wait
// rather than during the dial itself. A second dial call after that must
// never happen: the wait for the retry has to watch the parent context,
// not just sleep, or a client that left mid-backoff still gets retried on
// its behalf.
func TestDialTunnelNoRetryWhenClientCancelled(t *testing.T) {
	t.Parallel()
	logger, _ := newDialLogger()
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()

	var attempts atomic.Int32
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		if attempts.Add(1) > 1 {
			t.Error("dial called again after the client cancelled mid-backoff")
		}
		return nil, timeoutErr()
	}

	const backoff = 60 * time.Millisecond
	go func() {
		// Cancel partway through the backoff window between attempt 1 and
		// what would be attempt 2 — an MCP client tearing down while
		// spillway is waiting to retry, not while it is mid-dial.
		time.Sleep(backoff / 4)
		cancelParent()
	}()

	_, err := dialTunnel(parent, logger, dial, "example.invalid", "443", 10*time.Second, backoff)
	if err == nil {
		t.Fatal("dialTunnel: want error, got nil")
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1 (a client cancelled mid-backoff must not get a retry)", got)
	}
}

// TestDialTunnelNoRetryOnUnresolvableHost covers the other half of issue
// #221's retry boundary: "no such host" will not resolve differently 200ms
// later, so it must not cost a second attempt.
func TestDialTunnelNoRetryOnUnresolvableHost(t *testing.T) {
	t.Parallel()
	logger, _ := newDialLogger()

	var attempts atomic.Int32
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		attempts.Add(1)
		return nil, &net.DNSError{Err: "no such host", Name: addr, IsNotFound: true}
	}

	_, err := dialTunnel(context.Background(), logger, dial, "example.invalid", "443", 10*time.Second, time.Millisecond)
	if err == nil {
		t.Fatal("dialTunnel: want error, got nil")
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1 (an unresolvable host must not be retried)", got)
	}
}

// TestDialTunnelRetryStaysWithinBudget covers the wall-clock constraint:
// the retry must never make a client wait longer than the declared overall
// budget. The stub blocks until its ctx is done (as a real dial run to its
// deadline would) so the first attempt alone consumes the whole budget,
// leaving no time for the retry select to do anything but see ctx already
// done and return — proving the budget is shared across attempts, not
// multiplied by them.
func TestDialTunnelRetryStaysWithinBudget(t *testing.T) {
	t.Parallel()
	logger, _ := newDialLogger()

	const budget = 60 * time.Millisecond
	var attempts atomic.Int32
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		attempts.Add(1)
		<-ctx.Done()
		return nil, timeoutErr()
	}

	start := time.Now()
	_, err := dialTunnel(context.Background(), logger, dial, "example.invalid", "443", budget, 200*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("dialTunnel: want error, got nil")
	}

	// Generous slack for scheduling jitter, but nowhere near what a second
	// full attempt (let alone the backoff) would add if the budget were
	// per-attempt instead of overall.
	const slack = 100 * time.Millisecond
	if elapsed > budget+slack {
		t.Errorf("elapsed = %v, want <= budget(%v)+slack(%v) = %v", elapsed, budget, slack, budget+slack)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1 (the budget was spent by the first attempt, leaving none for a retry)", got)
	}
}
