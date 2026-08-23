package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/coderage-labs/spillway/internal/accounts"
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
func runEnv(cfg *config.Config, pemPath string) []string {
	proxy := "http://" + net.JoinHostPort(cfg.Proxy.Host, strconv.Itoa(cfg.Proxy.Port))
	env := append(os.Environ(),
		"HTTPS_PROXY="+proxy,
		"HTTP_PROXY="+proxy,
		"NO_PROXY=localhost,127.0.0.1,::1",
		"NODE_EXTRA_CA_CERTS="+pemPath,
	)
	for _, k := range stripEnv {
		env = slices.DeleteFunc(env, func(e string) bool {
			return strings.HasPrefix(e, k+"=")
		})
	}
	// Claude Code must wait out a pool-exhaustion hold for its first
	// response byte: raise its timeout past holdMax (+60s margin). Never
	// LOWER an existing value.
	if holdMax := cfg.PoolHoldMax(); holdMax > 0 {
		want := holdMax.Milliseconds() + 60_000
		existing, _ := strconv.ParseInt(os.Getenv("API_TIMEOUT_MS"), 10, 64)
		if existing < want {
			env = slices.DeleteFunc(env, func(e string) bool {
				return strings.HasPrefix(e, "API_TIMEOUT_MS=")
			})
			env = append(env, "API_TIMEOUT_MS="+strconv.FormatInt(want, 10))
		}
	}
	return env
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

// runClaude implements `spillway run`: spawn claude under the MITM proxy.
func runClaude(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := probeListening(cfg); err != nil {
		return err
	}

	pemPath, err := caPEMPath()
	if err != nil {
		return err
	}
	if _, err := mitm.EnsureCA(openSecrets(), pemPath, nil); err != nil {
		return fmt.Errorf("ensure MITM CA: %w", err)
	}

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
	return spawnCLI(bin, args, runEnv(cfg, pemPath))
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
