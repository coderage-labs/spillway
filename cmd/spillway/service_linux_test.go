//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSystemctl records the calls and returns canned results.
func fakeSystemctl(t *testing.T, fn func(args ...string) (string, error)) *[]string {
	t.Helper()
	var calls []string
	old := systemctl
	systemctl = func(args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		if fn == nil {
			return "", nil
		}
		return fn(args...)
	}
	t.Cleanup(func() { systemctl = old })
	return &calls
}

func TestUnitFileContents(t *testing.T) {
	u := unitFile("/opt/spillway/bin/spillway")

	if !strings.Contains(u, "ExecStart=/opt/spillway/bin/spillway server") {
		t.Errorf("unit does not run the binary it was given:\n%s", u)
	}
	// on-failure, not always: a clean exit is the daemon being asked to stop,
	// and restarting it then makes the service impossible to stop without
	// disabling it.
	if !strings.Contains(u, "Restart=on-failure") {
		t.Errorf("want Restart=on-failure:\n%s", u)
	}
	if strings.Contains(u, "Restart=always") {
		t.Errorf("Restart=always makes the service unstoppable:\n%s", u)
	}
	// default.target, not multi-user.target: this is a user unit, and
	// multi-user.target does not exist in the user manager, so the unit
	// would install and then never start at login.
	if !strings.Contains(u, "WantedBy=default.target") {
		t.Errorf("want WantedBy=default.target for a user unit:\n%s", u)
	}
}

func TestInstallWritesUnitAndEnablesIt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	calls := fakeSystemctl(t, nil)

	if err := requireSystemd(); err != "" {
		t.Skipf("no systemd here: %s", err)
	}
	if err := serviceInstall(); err != nil {
		t.Fatal(err)
	}

	unit := filepath.Join(dir, "systemd", "user", "spillway.service")
	b, err := os.ReadFile(unit)
	if err != nil {
		t.Fatalf("unit not written: %v", err)
	}
	if !strings.Contains(string(b), "ExecStart=") {
		t.Errorf("unit looks empty:\n%s", b)
	}

	joined := strings.Join(*calls, " | ")
	for _, want := range []string{"daemon-reload", "enable --now spillway.service"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q; calls were: %s", want, joined)
		}
	}
	// A reinstall over a running service has to swap the binary, which
	// enable alone does not do once the unit is already enabled.
	if !strings.Contains(joined, "restart spillway.service") {
		t.Errorf("reinstall does not restart, so an upgrade keeps the old binary: %s", joined)
	}
	// daemon-reload must come before enable, or systemd acts on the old unit.
	if idxOf(*calls, "daemon-reload") > idxOf(*calls, "enable --now spillway.service") {
		t.Errorf("daemon-reload must precede enable: %s", joined)
	}
}

func TestUninstallDisablesAndRemoves(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := requireSystemd(); err != "" {
		t.Skipf("no systemd here: %s", err)
	}
	unit := filepath.Join(dir, "systemd", "user", "spillway.service")
	if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unit, []byte(unitFile("/x")), 0o644); err != nil {
		t.Fatal(err)
	}

	calls := fakeSystemctl(t, nil)
	if err := serviceUninstall(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(unit); !os.IsNotExist(err) {
		t.Error("unit file left behind")
	}
	if !strings.Contains(strings.Join(*calls, " | "), "disable --now spillway.service") {
		t.Errorf("service not stopped: %v", *calls)
	}
}

// A failing disable must not stop the unit file being removed, or a
// half-uninstalled service can never be uninstalled.
func TestUninstallRemovesUnitEvenIfDisableFails(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := requireSystemd(); err != "" {
		t.Skipf("no systemd here: %s", err)
	}
	unit := filepath.Join(dir, "systemd", "user", "spillway.service")
	os.MkdirAll(filepath.Dir(unit), 0o755)
	os.WriteFile(unit, []byte("x"), 0o644)

	fakeSystemctl(t, func(args ...string) (string, error) {
		if args[0] == "disable" {
			return "Failed to disable unit", os.ErrPermission
		}
		return "", nil
	})
	if err := serviceUninstall(); err != nil {
		t.Fatalf("uninstall gave up after a failed disable: %v", err)
	}
	if _, err := os.Stat(unit); !os.IsNotExist(err) {
		t.Error("unit file left behind after a failed disable")
	}
}

func idxOf(all []string, want string) int {
	for i, s := range all {
		if strings.Contains(s, want) {
			return i
		}
	}
	return len(all)
}
