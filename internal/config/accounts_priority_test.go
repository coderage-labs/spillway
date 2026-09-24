package config

import (
	"path/filepath"
	"testing"
)

// Issue #169: an add used to leave priority unset, which means 0 — the top
// tier, alongside whatever was already there — so a brand-new account
// immediately competed for first-choice traffic. Each edge case below gets
// its own test on purpose: a single "it increments" test would pass while
// the gap and duplicate rules were both wrong.

// seedAccounts writes a config with the given name→priority accounts,
// bypassing the priority rule under test so the fixture says exactly what
// it means (including a hand-edited duplicate, which no add would produce).
func seedAccounts(t *testing.T, path string, prios map[string]int, order ...string) {
	t.Helper()
	cfg := Defaults()
	for _, n := range order {
		cfg.Accounts = append(cfg.Accounts, AccountConfig{
			Name: n, Type: "claude-oauth", Priority: prios[n],
		})
	}
	if err := validateAndWrite(path, &cfg); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// priorityOf reads one account's priority back off disk. Reading the file
// rather than trusting the return value is the point: the assigned number
// has to survive a restart, and the restart reads the yaml.
func priorityOf(t *testing.T, path, name string) int {
	t.Helper()
	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	for _, a := range cfg.Accounts {
		if a.Name == name {
			return a.Priority
		}
	}
	t.Fatalf("account %q not in config: %+v", name, cfg.Accounts)
	return 0
}

// TestNextPriorityEmptyPoolGetsZero: the first account IS the first choice.
func TestNextPriorityEmptyPoolGetsZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spillway.yaml")

	got, err := UpsertAccountWithPriority(path, AccountConfig{Name: "first", Type: "claude-oauth"}, nil)
	if err != nil {
		t.Fatalf("UpsertAccountWithPriority: %v", err)
	}
	if got != 0 {
		t.Errorf("first account into an empty pool: reported priority %d, want 0", got)
	}
	if on := priorityOf(t, path, "first"); on != 0 {
		t.Errorf("first account into an empty pool: config records priority %d, want 0", on)
	}
}

// TestNextPriorityGoesPastTheMaximumNotIntoTheGap is the sharp edge: with
// 5, 6, 9 in use the answer is 10. 7 would be "the lowest free number", and
// would silently promote a brand-new account above the one at 9 — the exact
// opposite of what adding a spare means.
func TestNextPriorityGoesPastTheMaximumNotIntoTheGap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spillway.yaml")
	seedAccounts(t, path, map[string]int{"a": 5, "b": 6, "c": 9}, "a", "b", "c")

	got, err := UpsertAccountWithPriority(path, AccountConfig{Name: "spare", Type: "claude-oauth"}, nil)
	if err != nil {
		t.Fatalf("UpsertAccountWithPriority: %v", err)
	}
	if got == 7 {
		t.Fatalf("new account filled the gap at 7 — that promotes it above %q at 9", "c")
	}
	if got != 10 {
		t.Fatalf("new account after 5,6,9: reported priority %d, want 10", got)
	}
	if on := priorityOf(t, path, "spare"); on != 10 {
		t.Errorf("new account after 5,6,9: config records priority %d, want 10", on)
	}
}

// TestNextPriorityIgnoresOrderOfTheMaximum: the maximum is not the last
// entry in the file. Written out of order so a "use the last account's
// priority" implementation cannot pass.
func TestNextPriorityIgnoresOrderOfTheMaximum(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spillway.yaml")
	seedAccounts(t, path, map[string]int{"top": 9, "mid": 4, "low": 1}, "top", "mid", "low")

	got, err := UpsertAccountWithPriority(path, AccountConfig{Name: "spare", Type: "claude-oauth"}, nil)
	if err != nil {
		t.Fatalf("UpsertAccountWithPriority: %v", err)
	}
	if got != 10 {
		t.Errorf("new account after 9,4,1 (in that file order): priority %d, want 10", got)
	}
}

// TestNextPriorityWithDuplicatesDoesNotRenumber: a hand-edited config with
// two accounts at 3 is the user's business. The add takes 4 and leaves both
// of them exactly where they were.
func TestNextPriorityWithDuplicatesDoesNotRenumber(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spillway.yaml")
	seedAccounts(t, path, map[string]int{"twin-a": 3, "twin-b": 3, "solo": 1}, "twin-a", "twin-b", "solo")

	got, err := UpsertAccountWithPriority(path, AccountConfig{Name: "spare", Type: "claude-oauth"}, nil)
	if err != nil {
		t.Fatalf("UpsertAccountWithPriority: %v", err)
	}
	if got != 4 {
		t.Errorf("new account after 3,3,1: priority %d, want 4", got)
	}
	for name, want := range map[string]int{"twin-a": 3, "twin-b": 3, "solo": 1} {
		if on := priorityOf(t, path, name); on != want {
			t.Errorf("add renumbered existing account %q: priority %d, want %d untouched", name, on, want)
		}
	}
}

// TestAddNeverMovesAnExistingAccount is the duplicate case's general form:
// after an ordinary add, every account that was already there still has the
// number it had. Separate from the duplicate test so a regression that only
// renumbers non-duplicates still fails something.
func TestAddNeverMovesAnExistingAccount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spillway.yaml")
	before := map[string]int{"work": 0, "side": 2, "reserve": 7}
	seedAccounts(t, path, before, "work", "side", "reserve")

	if _, err := UpsertAccountWithPriority(path, AccountConfig{Name: "spare", Type: "claude-oauth"}, nil); err != nil {
		t.Fatalf("UpsertAccountWithPriority: %v", err)
	}
	for name, want := range before {
		if on := priorityOf(t, path, name); on != want {
			t.Errorf("existing account %q moved: priority %d, want %d", name, on, want)
		}
	}
}

// TestExplicitPriorityWinsOverTheDefault: --priority is the whole escape
// hatch, and it has to beat a next-free that would otherwise be far away.
func TestExplicitPriorityWinsOverTheDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spillway.yaml")
	seedAccounts(t, path, map[string]int{"a": 5, "b": 9}, "a", "b")

	want := 2
	got, err := UpsertAccountWithPriority(path, AccountConfig{Name: "spare", Type: "claude-oauth"}, &want)
	if err != nil {
		t.Fatalf("UpsertAccountWithPriority: %v", err)
	}
	if got != want {
		t.Errorf("explicit --priority 2 with 5,9 in use: reported %d, want %d", got, want)
	}
	if on := priorityOf(t, path, "spare"); on != want {
		t.Errorf("explicit --priority 2: config records %d, want %d", on, want)
	}
}

// TestExplicitZeroPriorityIsNotTreatedAsUnset: "put this one at the front"
// is a real request, and the reason the explicit value is a pointer rather
// than a non-zero int. Without this, `--priority 0` would be silently
// upgraded to the back of the queue — the one place the default is exactly
// wrong.
func TestExplicitZeroPriorityIsNotTreatedAsUnset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spillway.yaml")
	seedAccounts(t, path, map[string]int{"a": 5, "b": 9}, "a", "b")

	zero := 0
	got, err := UpsertAccountWithPriority(path, AccountConfig{Name: "front", Type: "claude-oauth"}, &zero)
	if err != nil {
		t.Fatalf("UpsertAccountWithPriority: %v", err)
	}
	if got != 0 {
		t.Errorf("explicit --priority 0: reported %d, want 0", got)
	}
	if on := priorityOf(t, path, "front"); on != 0 {
		t.Errorf("explicit --priority 0: config records %d, want 0", on)
	}
}

// TestReloginKeepsExistingPriority: a re-auth is not an add. The account
// already has a place in the queue and OAuth learned nothing about it.
func TestReloginKeepsExistingPriority(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spillway.yaml")
	seedAccounts(t, path, map[string]int{"work": 6, "other": 9}, "work", "other")

	got, err := UpsertAccountWithPriority(path, AccountConfig{
		Name: "work", Type: "claude-oauth", ExpiresAt: 123, AccountUUID: "uuid-1",
	}, nil)
	if err != nil {
		t.Fatalf("UpsertAccountWithPriority: %v", err)
	}
	if got != 6 {
		t.Errorf("re-login reported priority %d, want the existing 6", got)
	}
	if on := priorityOf(t, path, "work"); on != 6 {
		t.Errorf("re-login moved the account: config records %d, want 6", on)
	}
}

// TestReloginWithExplicitPriorityRetunes: explicit wins on the re-auth path
// too — the one field a login payload is allowed to change.
func TestReloginWithExplicitPriorityRetunes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spillway.yaml")
	seedAccounts(t, path, map[string]int{"work": 6}, "work")

	want := 1
	got, err := UpsertAccountWithPriority(path, AccountConfig{
		Name: "work", Type: "claude-oauth", ExpiresAt: 123,
	}, &want)
	if err != nil {
		t.Fatalf("UpsertAccountWithPriority: %v", err)
	}
	if got != want {
		t.Errorf("re-login with --priority 1: reported %d, want %d", got, want)
	}
	if on := priorityOf(t, path, "work"); on != want {
		t.Errorf("re-login with --priority 1: config records %d, want %d", on, want)
	}
}

// TestUpsertAccountAppliesTheDefault pins the rule to the plain entry point
// as well, not just the priority-carrying one: UpsertAccount is what every
// other caller in the tree uses, and the default must not be something a
// caller has to opt into.
func TestUpsertAccountAppliesTheDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spillway.yaml")
	for i, name := range []string{"one", "two", "three"} {
		if err := UpsertAccount(path, AccountConfig{Name: name, Type: "claude-oauth"}); err != nil {
			t.Fatalf("UpsertAccount %q: %v", name, err)
		}
		if on := priorityOf(t, path, name); on != i {
			t.Errorf("account %q added %d-th: priority %d, want %d", name, i, on, i)
		}
	}
}

// TestNextPriorityRule covers the pure function directly, including shapes
// an add cannot easily produce (hand-edited negatives).
func TestNextPriorityRule(t *testing.T) {
	tests := []struct {
		name  string
		prios []int
		want  int
	}{
		{"empty pool", nil, 0},
		{"single zero", []int{0}, 1},
		{"contiguous", []int{0, 1, 2}, 3},
		{"gap", []int{5, 6, 9}, 10},
		{"duplicates", []int{3, 3}, 4},
		{"maximum first", []int{9, 1}, 10},
		{"all negative", []int{-5, -9}, -4},
		{"negative and zero", []int{-3, 0}, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var accts []AccountConfig
			for _, p := range tc.prios {
				accts = append(accts, AccountConfig{Priority: p})
			}
			if got := NextPriority(accts); got != tc.want {
				t.Errorf("NextPriority(%v) = %d, want %d", tc.prios, got, tc.want)
			}
		})
	}
}
