package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/coderage-labs/spillway/internal/accounts"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/coderage-labs/spillway/internal/admin"
	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/mitm"
)

// caPEMPath is where EnsureCA writes the CA cert clients must trust.
func caPEMPath() (string, error) {
	cfgPath, err := config.Path()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(cfgPath), "spillway-ca.pem"), nil
}

// stripEnv lists inherited vars that must NOT reach the child in MITM mode:
// a stale ANTHROPIC_BASE_URL would bypass the proxy entirely (an earlier
// deletes it in MITM mode too), and an inherited API key would drop claude
// out of subscription mode. ANTHROPIC_MODEL and friends stay.
var stripEnv = []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"}

// runEnv builds the MITM proxy env for a spawned CLI (design doc §6.15):
// HTTPS_PROXY/HTTP_PROXY at the spillway listener, NO_PROXY for loopback,
// NODE_EXTRA_CA_CERTS trusting our CA — never the system store.
//
// bundlePath, when non-empty, additionally points the non-Node TLS stacks a
// subprocess of the CLI might use (issue #64: an MCP server run under
// python/uv, most concretely) at a combined bundle — system roots plus
// spillway's CA — via SSL_CERT_FILE, REQUESTS_CA_BUNDLE and CURL_CA_BUNDLE.
// Every subprocess inherits HTTPS_PROXY/HTTP_PROXY transitively, but only
// Node was ever taught to trust spillway's CA; without this, any other
// language's client hitting a MITM'd host (api.anthropic.com) fails TLS
// verification, retries forever, and floods the log (11,665 lines measured
// live). When bundlePath is empty — mitm.EnsureCABundle couldn't confidently
// obtain this platform's system roots — none of the three are set: see the
// package doc on mitm.EnsureCABundle for why that is the safe choice.
func runEnv(cfg *config.Config, pemPath, bundlePath string) []string {
	env := os.Environ()
	spill := spillwayEnv(cfg, pemPath, bundlePath)
	env = append(env, withoutKey(spill, "API_TIMEOUT_MS")...)
	for _, k := range stripEnv {
		env = slices.DeleteFunc(env, func(e string) bool {
			return strings.HasPrefix(e, k+"=")
		})
	}
	// Claude Code must wait out a pool-exhaustion hold for its first
	// response byte: raise its timeout past holdMax (+60s margin). Never
	// LOWER an existing value.
	for _, kv := range spill {
		if v, ok := strings.CutPrefix(kv, "API_TIMEOUT_MS="); ok {
			want, _ := strconv.ParseInt(v, 10, 64)
			existing, _ := strconv.ParseInt(os.Getenv("API_TIMEOUT_MS"), 10, 64)
			if existing < want {
				env = slices.DeleteFunc(env, func(e string) bool {
					return strings.HasPrefix(e, "API_TIMEOUT_MS=")
				})
				env = append(env, kv)
			}
		}
	}
	return env
}

func withoutKey(kvs []string, key string) []string {
	return slices.DeleteFunc(slices.Clone(kvs), func(e string) bool {
		return strings.HasPrefix(e, key+"=")
	})
}

// spillwayEnv returns ONLY the variables spillway contributes, as KEY=value
// strings in a stable order. Shared by runEnv (spawned CLI) and `spillway
// env` (pasted into Claude Desktop) so the two cannot drift. API_TIMEOUT_MS
// is last and present only when holdMax > 0; it is the raw wanted value —
// runEnv additionally refuses to lower an inherited one.
func spillwayEnv(cfg *config.Config, pemPath, bundlePath string) []string {
	proxy := "http://" + net.JoinHostPort(cfg.Proxy.Host, strconv.Itoa(cfg.Proxy.Port))
	env := []string{
		"HTTPS_PROXY=" + proxy,
		"HTTP_PROXY=" + proxy,
		"NO_PROXY=localhost,127.0.0.1,::1",
		"NODE_EXTRA_CA_CERTS=" + pemPath,
	}
	if bundlePath != "" {
		env = append(env,
			"SSL_CERT_FILE="+bundlePath,
			"REQUESTS_CA_BUNDLE="+bundlePath,
			"CURL_CA_BUNDLE="+bundlePath,
		)
	}
	if holdMax := cfg.PoolHoldMax(); holdMax > 0 {
		env = append(env, "API_TIMEOUT_MS="+strconv.FormatInt(holdMax.Milliseconds()+60_000, 10))
	}
	return env
}

// runPrintEnv implements `spillway env`: print spillway's variables, one
// unquoted KEY=value per line, for pasting into Claude Desktop's local
// environment editor. Never mints a CA, never writes a file, never touches
// the keychain or the daemon.
func runPrintEnv(w io.Writer) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	pemPath, err := caPEMPath()
	if err != nil {
		return err
	}
	return printEnv(w, cfg, pemPath)
}

func printEnv(w io.Writer, cfg *config.Config, pemPath string) error {
	if _, err := os.Stat(pemPath); err != nil {
		return fmt.Errorf("MITM CA not found at %s — start the daemon first (`spillway server` or `spillway service install`); a CA minted here would not match the one the daemon serves", pemPath)
	}
	bundlePath := mitm.BundlePath(pemPath)
	if _, err := os.Stat(bundlePath); err != nil {
		bundlePath = ""
	}
	for _, kv := range spillwayEnv(cfg, pemPath, bundlePath) {
		fmt.Fprintln(w, kv)
	}
	return nil
}

// probeListening refuses to launch when the proxy isn't up — a direct
// launch would silently bypass the pool.
func probeListening(cfg *config.Config) error {
	addr := net.JoinHostPort(cfg.Proxy.Host, strconv.Itoa(cfg.Proxy.Port))
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return fmt.Errorf("spillway proxy not listening on %s — start it first (`spillway server`); launching claude directly would bypass the pool", addr)
	}
	conn.Close()
	return nil
}

// runWaitTimeout bounds how long `spillway run` waits for the daemon, across
// both "not listening yet" (launchd has not started it in the first seconds
// after login) and "listening but still initialising" (#259). Three minutes
// covers the slowest measured login-time init (~2 min) with margin, while
// staying inside the daemon's own 5-minute init deadline.
const runWaitTimeout = 3 * time.Minute

// readyProbe is what waitReady observes. Split out so tests can drive it.
type readyProbe struct {
	// listening reports whether the proxy port accepts a connection.
	listening func() error
	// status reads /api/state's "status" ("" from a daemon older than #259).
	status func() (string, error)
}

// daemonProbe builds the production probe: a TCP dial of the proxy port and
// the admin client every other CLI command uses (unix socket, token file).
func daemonProbe(cfg *config.Config) readyProbe {
	return readyProbe{
		listening: func() error { return probeListening(cfg) },
		status: func() (string, error) {
			api, err := dialAdmin()
			if err != nil {
				return "", err
			}
			var st struct {
				Status string `json:"status"`
			}
			if err := api.get("/api/state", &st); err != nil {
				return "", err
			}
			return st.Status, nil
		},
	}
}

// waitReady blocks until the daemon is listening AND reports ready.
//
// Since #259 the port binds before the daemon's CA step, so "listening" no
// longer means the CA on disk is final. Touching the CA before ready would
// mint one the daemon then regenerates, and claude would load a stale
// NODE_EXTRA_CA_CERTS anchor (the #66 stranding). It also tolerates the
// daemon not being up at all yet, which is the normal state in the first
// seconds after login. Stops at ctx cancellation (Ctrl-C) or timeout; the
// timeout error is the usual "not listening" refusal when that is the state.
func waitReady(ctx context.Context, p readyProbe, errOut io.Writer, timeout, interval time.Duration) error {
	deadline := time.Now().Add(timeout)
	lastMsg := ""
	say := func(m string) {
		if m != lastMsg {
			fmt.Fprintln(errOut, "spillway: "+m)
			lastMsg = m
		}
	}
	var lastErr error
	for {
		if lerr := p.listening(); lerr != nil {
			lastErr = lerr
			say("waiting for spillway to start…")
		} else {
			status, err := p.status()
			switch {
			case err == nil && (status == "" || status == admin.StatusReady):
				return nil
			case err == nil:
				lastErr = fmt.Errorf("spillway is still starting (status %q)", status)
				say("waiting for spillway to finish starting…")
			case errors.Is(err, ErrAdminUnreachable):
				// Admin binds before the proxy, so this is a daemon mid-restart.
				lastErr = err
				say("waiting for spillway to start…")
			default:
				return err // a real answer (e.g. 401): waiting will not fix it
			}
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("gave up waiting %s: %w", timeout.Round(time.Second), lastErr)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("interrupted while waiting for spillway: %w", ctx.Err())
		case <-time.After(interval):
		}
	}
}

// prepareCA waits for the daemon to be ready, and only then reads (or, if
// somehow absent, creates) the CA pem the spawned CLI will trust.
func prepareCA(ctx context.Context, p readyProbe, pemPath string, errOut io.Writer, timeout, interval time.Duration) (*mitm.CA, error) {
	if err := waitReady(ctx, p, errOut, timeout, interval); err != nil {
		return nil, err
	}
	ca, err := mitm.EnsureCA(pemPath, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("ensure MITM CA: %w", err)
	}
	return ca, nil
}

// runClaude implements `spillway run`: spawn claude under the MITM proxy.
func runClaude(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	pemPath, err := caPEMPath()
	if err != nil {
		return err
	}
	// Wait for the daemon to be listening AND ready before touching the CA:
	// it mints the real chain for its actual host set as its last init step,
	// and a CA read earlier would be one it then replaces (see waitReady).
	// Ctrl-C stops the wait; spawnCLI installs its own handler afterwards.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	ca, err := prepareCA(ctx, daemonProbe(cfg), pemPath, os.Stderr, runWaitTimeout, time.Second)
	stop()
	if err != nil {
		return err
	}
	// Best-effort (issue #64): a combined system-roots + spillway-CA bundle
	// for non-Node subprocesses. bundlePath is "" when this platform's
	// system roots can't be confidently found — runEnv then leaves
	// SSL_CERT_FILE/REQUESTS_CA_BUNDLE/CURL_CA_BUNDLE unset, same as today.
	bundlePath, _ := mitm.EnsureCABundle(pemPath, ca.CertPEM(), nil)

	// Strip a leading "--" separator.
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	bin := os.Getenv("SPILLWAY_CLAUDE_BIN")
	if bin == "" {
		bin = "claude"
	}
	// §6.8c: warn, never block. The CLI auto-updates, and refusing to launch
	// because it moved would be worse than the drift being guarded against —
	// the proxy usually still works.
	if os.Getenv("SPILLWAY_ALLOW_UNTESTED_CLIENT") == "" {
		if w := accounts.CheckClientVersion(claudeVersion(bin)); w != "" {
			fmt.Fprintln(os.Stderr, "spillway: "+w)
			fmt.Fprintln(os.Stderr, "spillway: set SPILLWAY_ALLOW_UNTESTED_CLIENT=1 to silence this")
		}
	}
	return spawnCLI(bin, args, runEnv(cfg, pemPath, bundlePath))
}

// claudeVersion asks the CLI what it is. Best-effort: an empty answer means
// no check, never a failure to launch.
func claudeVersion(bin string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return ""
	}
	// "2.1.0 (Claude Code)" -> "2.1.0"
	return strings.Fields(strings.TrimSpace(string(out)))[0]
}

// spawnCLI execs bin with stdio inherited, forwards SIGINT/SIGTERM to the
// child, and propagates its exit code.
func spawnCLI(bin string, args []string, env []string) error {
	cmd := exec.Command(bin, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = env

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", bin, err)
	}

	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	go func() {
		for s := range sig {
			if cmd.Process != nil {
				_ = cmd.Process.Signal(s)
			}
		}
	}()

	err := cmd.Wait()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return &ExitCodeError{Code: exitErr.ExitCode()}
	}
	return err
}

// ExitCodeError carries a child's exit code so main can propagate it.
type ExitCodeError struct{ Code int }

func (e *ExitCodeError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }
