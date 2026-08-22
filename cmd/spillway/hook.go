package main

// `spillway hook` wires spillway into every `claude` launch by writing an
// `env` block into ~/.claude/settings.json — not a shell alias (design doc
// §6.14). An alias is only seen by interactive shells, so IDE extensions,
// scripts and tmux-spawned sessions silently bypass the pool; settings.json
// is honoured however claude is started, is scoped to claude alone, and is
// reversible.
//
// The block installs the MITM env (§6.15): Remote Control refuses to start
// when ANTHROPIC_BASE_URL points anywhere but api.anthropic.com, so a
// base-URL hook would break RC. HTTPS_PROXY + NODE_EXTRA_CA_CERTS keep the
// base URL intact while still routing through the pool.

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/coderage-labs/spillway/internal/config"
)

// managedKeys are the env vars spillway owns inside settings.json. Uninstall
// removes exactly these and nothing else, so hand-added vars survive.
var managedKeys = []string{
	"HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "NODE_EXTRA_CA_CERTS", "API_TIMEOUT_MS",
}

func claudeSettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

// hookEnv is the env block spillway manages, derived from the live config.
func hookEnv(cfg *config.Config, pemPath string) map[string]string {
	proxy := "http://" + net.JoinHostPort(cfg.Proxy.Host, strconv.Itoa(cfg.Proxy.Port))
	env := map[string]string{
		"HTTPS_PROXY":         proxy,
		"HTTP_PROXY":          proxy,
		"NO_PROXY":            "localhost,127.0.0.1,::1",
		"NODE_EXTRA_CA_CERTS": pemPath,
	}
	if holdMax := cfg.PoolHoldMax(); holdMax > 0 {
		env["API_TIMEOUT_MS"] = strconv.FormatInt(holdMax.Milliseconds()+60_000, 10)
	}
	return env
}

// mergeHook returns settings with spillway's env keys applied. Unknown
// top-level keys and unmanaged env vars are preserved byte-for-byte in
// value; only the managed keys are written.
func mergeHook(settings map[string]any, env map[string]string) map[string]any {
	if settings == nil {
		settings = map[string]any{}
	}
	cur, _ := settings["env"].(map[string]any)
	if cur == nil {
		cur = map[string]any{}
	}
	for k, v := range env {
		cur[k] = v
	}
	settings["env"] = cur
	return settings
}

// removeHook strips only spillway's managed keys, dropping an empty env.
func removeHook(settings map[string]any) map[string]any {
	cur, _ := settings["env"].(map[string]any)
	if cur == nil {
		return settings
	}
	for _, k := range managedKeys {
		delete(cur, k)
	}
	if len(cur) == 0 {
		delete(settings, "env")
	} else {
		settings["env"] = cur
	}
	return settings
}

func readSettings(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		// Never clobber a file we cannot parse — the user's settings are not
		// ours to rewrite on a guess.
		return nil, fmt.Errorf("parse %s: %w (fix or move the file, then retry)", path, err)
	}
	return m, nil
}

// writeSettings writes atomically, keeping a .bak of any previous content.
func writeSettings(path string, m map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if prev, err := os.ReadFile(path); err == nil {
		if err := os.WriteFile(path+".spillway.bak", prev, 0o600); err != nil {
			return fmt.Errorf("write backup: %w", err)
		}
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func runHook(args []string) error {
	action := "status"
	if len(args) > 0 {
		action = args[0]
	}
	path, err := claudeSettingsPath()
	if err != nil {
		return err
	}
	switch action {
	case "install":
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		pem, err := caPEMPath()
		if err != nil {
			return err
		}
		if _, err := os.Stat(pem); err != nil {
			return fmt.Errorf("CA not found at %s — start `spillway server` once to generate it", pem)
		}
		m, err := readSettings(path)
		if err != nil {
			return err
		}
		if err := writeSettings(path, mergeHook(m, hookEnv(cfg, pem))); err != nil {
			return err
		}
		fmt.Printf("hook installed in %s\n", path)
		fmt.Println("every `claude` launch now routes through spillway (restart running sessions)")
		return nil
	case "uninstall":
		m, err := readSettings(path)
		if err != nil {
			return err
		}
		if err := writeSettings(path, removeHook(m)); err != nil {
			return err
		}
		fmt.Printf("hook removed from %s\n", path)
		return nil
	case "status":
		m, err := readSettings(path)
		if err != nil {
			return err
		}
		cur, _ := m["env"].(map[string]any)
		var found []string
		for _, k := range managedKeys {
			if v, ok := cur[k]; ok {
				found = append(found, fmt.Sprintf("  %s=%v", k, v))
			}
		}
		if len(found) == 0 {
			fmt.Printf("hook NOT installed (%s)\n", path)
			return nil
		}
		sort.Strings(found)
		fmt.Printf("hook installed (%s):\n", path)
		for _, l := range found {
			fmt.Println(l)
		}
		return nil
	default:
		return fmt.Errorf("unknown hook action %q (install|uninstall|status)", action)
	}
}
