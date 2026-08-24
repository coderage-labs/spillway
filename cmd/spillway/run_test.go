package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/coderage-labs/spillway/internal/config"
)

func TestRunEnv(t *testing.T) {
	cfg := config.Defaults()
	env := runEnv(&cfg, "/home/u/.config/spillway-ca.pem", "")
	joined := strings.Join(env, "\n")
	for _, want := range []string{
		"HTTPS_PROXY=http://127.0.0.1:7654",
		"HTTP_PROXY=http://127.0.0.1:7654",
		"NO_PROXY=localhost,127.0.0.1,::1",
		"NODE_EXTRA_CA_CERTS=/home/u/.config/spillway-ca.pem",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("env missing %q", want)
		}
	}
}

// TestRunEnvSetsCABundleVarsWhenBundlePresent covers issue #64: when a
// combined CA bundle exists, all three non-Node TLS variables must point at
// it, and NODE_EXTRA_CA_CERTS must be untouched (Node keeps trusting the
// plain CA cert, not the bundle).
func TestRunEnvSetsCABundleVarsWhenBundlePresent(t *testing.T) {
	cfg := config.Defaults()
	env := runEnv(&cfg, "/tmp/ca.pem", "/tmp/ca-bundle.pem")
	joined := strings.Join(env, "\n")
	for _, want := range []string{
		"SSL_CERT_FILE=/tmp/ca-bundle.pem",
		"REQUESTS_CA_BUNDLE=/tmp/ca-bundle.pem",
		"CURL_CA_BUNDLE=/tmp/ca-bundle.pem",
		"NODE_EXTRA_CA_CERTS=/tmp/ca.pem",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("env missing %q", want)
		}
	}
}

// TestRunEnvOmitsCABundleVarsWhenBundleAbsent is the other half of the trap
// in issue #64: an empty bundlePath (mitm.EnsureCABundle couldn't confidently
// find this platform's system roots) must leave SSL_CERT_FILE,
// REQUESTS_CA_BUNDLE and CURL_CA_BUNDLE unset entirely — never pointed at a
// bundle missing the system roots, which would break verification of every
// ordinary site for anything reading those variables.
func TestRunEnvOmitsCABundleVarsWhenBundleAbsent(t *testing.T) {
	cfg := config.Defaults()
	env := runEnv(&cfg, "/tmp/ca.pem", "")
	joined := strings.Join(env, "\n")
	for _, banned := range []string{"SSL_CERT_FILE=", "REQUESTS_CA_BUNDLE=", "CURL_CA_BUNDLE="} {
		if strings.Contains(joined, banned) {
			t.Errorf("env must not contain %q when no bundle exists", banned)
		}
	}
	if !strings.Contains(joined, "NODE_EXTRA_CA_CERTS=/tmp/ca.pem") {
		t.Error("NODE_EXTRA_CA_CERTS should still be set")
	}
}

func TestRunEnvAPITimeoutBump(t *testing.T) {
	cfg := config.Defaults() // holdMax 4h → 4h+60s = 14460000ms

	env := runEnv(&cfg, "/tmp/ca.pem", "")
	if !strings.Contains(strings.Join(env, "\n"), "API_TIMEOUT_MS=14460000") {
		t.Error("API_TIMEOUT_MS not set to holdMax+60s")
	}

	// Never lower an existing higher value.
	t.Setenv("API_TIMEOUT_MS", "20000000")
	env = runEnv(&cfg, "/tmp/ca.pem", "")
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "API_TIMEOUT_MS=20000000") {
		t.Error("existing higher API_TIMEOUT_MS was lowered")
	}

	// An existing LOWER value is raised.
	t.Setenv("API_TIMEOUT_MS", "1000")
	env = runEnv(&cfg, "/tmp/ca.pem", "")
	joined = strings.Join(env, "\n")
	if !strings.Contains(joined, "API_TIMEOUT_MS=14460000") || strings.Count(joined, "API_TIMEOUT_MS=") != 1 {
		t.Errorf("existing lower API_TIMEOUT_MS not replaced exactly once:\n%s", joined)
	}

	// holdMax 0 → no bump; an inherited value passes through untouched.
	cfg.Pool.HoldMax = "0"
	env = runEnv(&cfg, "/tmp/ca.pem", "")
	joined = strings.Join(env, "\n")
	if strings.Contains(joined, "API_TIMEOUT_MS=14460000") {
		t.Error("API_TIMEOUT_MS bumped despite holdMax 0")
	}
	if !strings.Contains(joined, "API_TIMEOUT_MS=1000") {
		t.Error("inherited API_TIMEOUT_MS lost with holdMax 0")
	}
}

func TestRunEnvStripsAnthropicKnobs(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "https://stale.example.com")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-api-key")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "tok")
	t.Setenv("ANTHROPIC_MODEL", "claude-sonnet-4-5")

	cfg := config.Defaults()
	env := runEnv(&cfg, "/tmp/ca.pem", "")
	joined := strings.Join(env, "\n")
	for _, banned := range []string{"ANTHROPIC_BASE_URL=", "ANTHROPIC_API_KEY=", "ANTHROPIC_AUTH_TOKEN="} {
		if strings.Contains(joined, banned) {
			t.Errorf("env contains banned var %q", banned)
		}
	}
	if !strings.Contains(joined, "ANTHROPIC_MODEL=claude-sonnet-4-5") {
		t.Error("ANTHROPIC_MODEL should be left alone")
	}
}

func TestProbeListeningRefusesWhenDown(t *testing.T) {
	cfg := config.Defaults()
	cfg.Proxy.Port = 1 // nothing listens here
	err := probeListening(&cfg)
	if err == nil || !strings.Contains(err.Error(), "not listening") {
		t.Errorf("err = %v, want refusal", err)
	}
}

// spawnCLI with a fake claude: env is inherited (minus stripped Anthropic
// knobs) and the exit code propagates.
func TestSpawnCLIFakeClaude(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script")
	}
	t.Setenv("ANTHROPIC_BASE_URL", "https://stale.example.com")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-api-key")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "tok")

	dir := t.TempDir()
	outFile := filepath.Join(dir, "env.out")
	fake := filepath.Join(dir, "claude")
	script := "#!/bin/sh\nenv > \"$FAKE_OUT\"\nexit 7\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := config.Defaults()
	env := append(runEnv(&cfg, "/tmp/ca.pem", ""), "FAKE_OUT="+outFile)
	err := spawnCLI(fake, []string{"--version"}, env)
	var exitErr *ExitCodeError
	if !errors.As(err, &exitErr) || exitErr.Code != 7 {
		t.Fatalf("err = %v, want exit code 7", err)
	}

	out, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"HTTPS_PROXY=http://127.0.0.1:7654", "NODE_EXTRA_CA_CERTS=/tmp/ca.pem"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("child env missing %q", want)
		}
	}
	for _, banned := range []string{"ANTHROPIC_BASE_URL=", "ANTHROPIC_API_KEY=", "ANTHROPIC_AUTH_TOKEN="} {
		if strings.Contains(string(out), banned) {
			t.Errorf("child env contains banned var %q", banned)
		}
	}
}
