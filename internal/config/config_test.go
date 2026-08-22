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
	yaml := "proxy:\n  port: 9000\n  host: 0.0.0.0\nupstream: https://example.com\nlog:\n  level: debug\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if cfg.Proxy.Port != 9000 || cfg.Proxy.Host != "0.0.0.0" {
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
