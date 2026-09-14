package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/coderage-labs/spillway/internal/testmode"
)

func TestLoadCreatesDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spillway.yaml")

	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}

	want := Defaults()
	if !reflect.DeepEqual(*cfg, want) {
		t.Errorf("got %+v, want defaults %+v", *cfg, want)
	}

	testmode.AssertPrivateFile(t, path)
}

func TestLoadReadsExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spillway.yaml")
	// allowRemote is required for the 0.0.0.0 bind to load at all — see
	// TestValidateRefusesNonLoopbackProxyHost.
	yaml := "proxy:\n  port: 9000\n  host: 0.0.0.0\n  allowRemote: true\nupstream: https://example.com\nlog:\n  level: debug\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if cfg.Proxy.Port != 9000 || cfg.Proxy.Host != "0.0.0.0" || !cfg.Proxy.AllowRemote {
		t.Errorf("proxy = %+v", cfg.Proxy)
	}
	if cfg.Upstream != "https://example.com" {
		t.Errorf("upstream = %q", cfg.Upstream)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("log.level = %q", cfg.Log.Level)
	}
}

func TestLoadInvalidYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spillway.yaml")
	if err := os.WriteFile(path, []byte("{not yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFrom(path); err == nil || !strings.Contains(err.Error(), "parse config") {
		t.Errorf("err = %v, want parse error", err)
	}
}

func TestValidationErrors(t *testing.T) {
	cases := []struct {
		name  string
		yaml  string
		wantE string
	}{
		{"bad port", "proxy:\n  port: 0\n", "proxy.port"},
		{"port too high", "proxy:\n  port: 70000\n", "proxy.port"},
		{"empty host", "proxy:\n  host: \"\"\n", "proxy.host"},
		{"bad upstream", "upstream: \"not a url\"\n", "upstream"},
		{"bad scheme", "upstream: \"ftp://example.com\"\n", "scheme"},
		{"bad level", "log:\n  level: verbose\n", "log.level"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "spillway.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadFrom(path)
			if err == nil || !strings.Contains(err.Error(), tc.wantE) {
				t.Errorf("err = %v, want it to mention %q", err, tc.wantE)
			}
		})
	}
}

func TestPartialConfigKeepsDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spillway.yaml")
	if err := os.WriteFile(path, []byte("proxy:\n  port: 1234\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if cfg.Proxy.Port != 1234 {
		t.Errorf("port = %d", cfg.Proxy.Port)
	}
	d := Defaults()
	if cfg.Upstream != d.Upstream || cfg.Log.Level != d.Log.Level || cfg.Proxy.Host != d.Proxy.Host {
		t.Errorf("unset fields not defaulted: %+v", cfg)
	}
}

// TestValidateRefusesNonLoopbackProxyHost is the guard for issue #12: the
// proxy listener is unauthenticated and injects a pooled account's credential
// into everything it forwards, so a bind that reaches off this machine must
// be refused unless the operator explicitly opts in. Both columns matter —
// the "with opt-in" side is what proves the guard is a gate and not a
// blanket ban, and the loopback rows are what prove a correct config still
// starts.
func TestValidateRefusesNonLoopbackProxyHost(t *testing.T) {
	cases := []struct {
		name     string
		host     string
		loopback bool // want: accepted with allowRemote unset
	}{
		{"ipv4 loopback", "127.0.0.1", true},
		{"ipv4 loopback elsewhere in 127/8", "127.0.0.53", true},
		{"localhost", "localhost", true},
		{"ipv6 loopback", "::1", true},
		{"ipv6 loopback bracketed", "[::1]", true},
		{"ipv4 unspecified", "0.0.0.0", false},
		{"ipv6 unspecified", "::", false},
		{"lan address", "192.168.1.10", false},
		{"public address", "203.0.113.7", false},
		{"hostname", "proxy.internal.example.com", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Defaults()
			c.Proxy.Host = tc.host

			err := c.Validate()
			if tc.loopback && err != nil {
				t.Fatalf("host %q without allowRemote: Validate = %v, want nil "+
					"(a loopback bind must still start)", tc.host, err)
			}
			if !tc.loopback {
				if err == nil {
					t.Fatalf("host %q without allowRemote: Validate = nil, want refusal — "+
						"this binds the credential-injecting proxy off the machine", tc.host)
				}
				// The message has to name the field and the escape hatch, or
				// the operator cannot act on it.
				for _, want := range []string{"proxy.host", tc.host, "allowRemote"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("host %q: error %q does not mention %q", tc.host, err, want)
					}
				}
			}

			// The opt-in must make every host acceptable, loopback included.
			c.Proxy.AllowRemote = true
			if err := c.Validate(); err != nil {
				t.Errorf("host %q with allowRemote: Validate = %v, want nil", tc.host, err)
			}
		})
	}
}

// An empty host is rejected for being empty, before the loopback question is
// even asked — and allowRemote must not turn that into an accepted config,
// since an empty host binds the wildcard just as 0.0.0.0 does.
func TestValidateRejectsEmptyProxyHostEvenWithAllowRemote(t *testing.T) {
	for _, allowRemote := range []bool{false, true} {
		c := Defaults()
		c.Proxy.Host = ""
		c.Proxy.AllowRemote = allowRemote
		err := c.Validate()
		if err == nil {
			t.Fatalf("allowRemote=%v: empty proxy.host accepted, want refusal", allowRemote)
		}
		if !strings.Contains(err.Error(), "proxy.host") {
			t.Errorf("allowRemote=%v: error %q does not mention proxy.host", allowRemote, err)
		}
	}
}

// The shipped default must survive its own guard. A guard that fails closed
// on a correct config is worse than the hole it plugs.
func TestDefaultsBindLoopbackAndValidate(t *testing.T) {
	c := Defaults()
	if c.Proxy.AllowRemote {
		t.Error("Defaults() opts in to a remote bind; the default must be closed")
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Defaults().Validate() = %v, want nil", err)
	}
}

// A config file written before #171 has no log.maxSizeMB and no
// log.maxFiles. Those files are on every machine that already runs the
// daemon, and the bound has to arrive for them without anyone editing
// anything — a default of zero would read as "no bound", which is the bug.
func TestExistingConfigsInheritTheLogBound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spillway.yaml")
	// Exactly what a pre-#171 file's log block looks like.
	if err := os.WriteFile(path, []byte("log:\n  level: info\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if cfg.Log.MaxSizeMB != Defaults().Log.MaxSizeMB {
		t.Errorf("log.maxSizeMB = %d for a config that predates the key; want the default %d",
			cfg.Log.MaxSizeMB, Defaults().Log.MaxSizeMB)
	}
	if cfg.Log.MaxFiles != Defaults().Log.MaxFiles {
		t.Errorf("log.maxFiles = %d for a config that predates the key; want the default %d",
			cfg.Log.MaxFiles, Defaults().Log.MaxFiles)
	}
	if cfg.Log.MaxSizeMB < 1 || cfg.Log.MaxFiles < 1 {
		t.Fatal("the inherited bound does not bound anything")
	}
}

// An explicit setting is honoured — the knob is a knob, not decoration.
func TestLogBoundIsConfigurable(t *testing.T) {
	cfg, err := ParseValidate([]byte("log:\n  level: info\n  maxSizeMB: 2\n  maxFiles: 3\n"))
	if err != nil {
		t.Fatalf("ParseValidate: %v", err)
	}
	if cfg.Log.MaxSizeMB != 2 || cfg.Log.MaxFiles != 3 {
		t.Fatalf("log bound = %d MB x %d files; want the configured 2 MB x 3",
			cfg.Log.MaxSizeMB, cfg.Log.MaxFiles)
	}
}

// Zero must be refused rather than quietly meaning "unlimited": an
// unbounded log file is the defect, so the config cannot have a spelling
// that restores it by accident.
func TestValidateRejectsAnUnboundedLog(t *testing.T) {
	for _, tc := range []struct{ name, yaml, want string }{
		{"zero size", "log:\n  level: info\n  maxSizeMB: 0\n", "log.maxSizeMB"},
		{"negative size", "log:\n  level: info\n  maxSizeMB: -1\n", "log.maxSizeMB"},
		{"zero generations", "log:\n  level: info\n  maxFiles: 0\n", "log.maxFiles"},
		{"negative generations", "log:\n  level: info\n  maxFiles: -2\n", "log.maxFiles"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseValidate([]byte(tc.yaml))
			if err == nil {
				t.Fatal("accepted a log configuration that bounds nothing")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %s", err, tc.want)
			}
		})
	}
}
