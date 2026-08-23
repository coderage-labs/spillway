package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coderage-labs/spillway/internal/config"
)

// fixture accounts for the resolver table below (#23). Each one earns its
// place by making some tier load-bearing rather than redundant with the
// fallback prefix/substring tier — a query that already resolves uniquely
// through the fallback proves nothing about the exact-match tiers above it.
//
//   - "kimi" is a full account name that is ALSO a substring of
//     "kimi-standby"'s name, so an exact-name query only resolves uniquely
//     if the exact-name tier fires before the fallback would find both and
//     call it ambiguous.
//   - "ckitch@main.example", labelled "Arena", carries a name unrelated to
//     the label, so a query for the label only ever matches through the
//     label — and the label's mixed case means it only matches a
//     lowercase query if both sides get folded, not just the query.
//   - "ckitch@work.example", labelled "work", and "ckitch@work2.example",
//     labelled "work-backup", sit next to each other so a short query is
//     ambiguous between them unless a tier above resolves it outright.
func resolverFixture() []switchAccount {
	return []switchAccount{
		{Name: "kimi"},
		{Name: "kimi-standby"},
		{Name: "ckitch@main.example", Label: "Arena"},
		{Name: "ckitch@work.example", Label: "work"},
		{Name: "ckitch@work2.example", Label: "work-backup"},
	}
}

func TestResolveAccountName(t *testing.T) {
	accounts := resolverFixture()
	for _, tc := range []struct {
		name  string
		query string
		want  string // resolved account name; "" means an error is expected
	}{
		// Without the exact-name tier, "kimi" also substring-matches
		// "kimi-standby" and the fallback would call this ambiguous.
		{"exact name", "kimi", "kimi"},
		// Without the exact-label tier, "work" also substring-matches the
		// "work-backup" label and account, and the fallback would call
		// this ambiguous too.
		{"exact label", "work", "ckitch@work.example"},
		{"prefix", "kimi-sta", "kimi-standby"},
		{"substring", "standby", "kimi-standby"},
		// The stored label is "Arena", with a name that shares none of its
		// letters — this only resolves if both the query and the label get
		// folded to the same case before comparing.
		{"differing case", "arena", "ckitch@main.example"},
		{"ambiguous", "wor", ""},
		{"unknown", "nope", ""},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveAccountName(accounts, tc.query)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("resolveAccountName(%q) = %q, nil; want an error", tc.query, got)
				}
				if got != "" {
					t.Errorf("resolveAccountName(%q) returned %q alongside an error; must resolve nothing on failure", tc.query, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveAccountName(%q) unexpected error: %v", tc.query, err)
			}
			if got != tc.want {
				t.Errorf("resolveAccountName(%q) = %q, want %q", tc.query, got, tc.want)
			}
		})
	}
}

// The ambiguous case above only checks that resolution refuses to pick one.
// This checks the error actually names both candidates — the thing a user
// needs to type a more specific query — rather than just saying "ambiguous".
func TestResolveAccountNameAmbiguityNamesBothCandidates(t *testing.T) {
	accounts := resolverFixture()
	_, err := resolveAccountName(accounts, "wor")
	if err == nil {
		t.Fatal("want an error for an ambiguous query, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"ckitch@work.example", "ckitch@work2.example"} {
		if !strings.Contains(msg, want) {
			t.Errorf("ambiguity error does not name %q:\n%s", want, msg)
		}
	}
}

// Unknown input should say what exists, not just that nothing matched — the
// reader has to type something next.
func TestResolveAccountNameUnknownListsWhatExists(t *testing.T) {
	accounts := resolverFixture()
	_, err := resolveAccountName(accounts, "nope")
	if err == nil {
		t.Fatal("want an error for an unknown query, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"kimi", "ckitch@main.example"} {
		if !strings.Contains(msg, want) {
			t.Errorf("unknown-account error does not list %q among what exists:\n%s", want, msg)
		}
	}
}

func TestResolveAccountNameNoAccountsConfigured(t *testing.T) {
	_, err := resolveAccountName(nil, "arena")
	if err == nil {
		t.Fatal("want an error when no accounts are configured, got nil")
	}
	// Not just "no match" — with zero accounts the generic no-match message
	// ("no account matches ... have: ") would technically still be an
	// error, but it is the wrong error: it implies accounts exist and none
	// fit, rather than that there is nothing to switch to at all.
	if !strings.Contains(err.Error(), "no accounts") {
		t.Errorf("error does not say no accounts are configured: %q", err.Error())
	}
}

// An empty query must never resolve, even when there is exactly one account
// to (mis)match it against — the general ambiguity check only fires past
// one candidate, so a single-account pool is the case that would otherwise
// let an empty string silently resolve.
func TestResolveAccountNameEmptyQueryNeverAutoResolves(t *testing.T) {
	accounts := []switchAccount{{Name: "only-one"}}
	got, err := resolveAccountName(accounts, "")
	if err == nil {
		t.Fatalf("resolveAccountName(\"\") = %q, nil; want an error even with a single account", got)
	}
	if got != "" {
		t.Errorf("resolveAccountName(\"\") returned %q alongside an error", got)
	}
}

// --- #44: the config-backed source, and the per-command resolution these
// four commands actually call — login, accounts remove, accounts priority
// and accounts overage all work from the config file with no daemon
// running, so these tests write a real scratch config and resolve against
// it, the same way runAccounts and runLoginClaude/Kimi do. This is the
// layer analogous to parseSwitchArgs in switch_test.go: testing through
// runAccounts or runLoginClaude directly would call openSecrets(), which on
// a machine with a real OS keychain reaches it for real — exactly the kind
// of dependency this split exists to avoid, the same way runSwitch dialing
// the daemon would.

// configFixture mirrors resolverFixture's shape but as the config records
// login/accounts actually read, written to a scratch spillway.yaml.
func configFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "spillway.yaml")
	for _, a := range []config.AccountConfig{
		{Name: "kimi", Type: "claude-oauth"},
		{Name: "kimi-standby", Type: "claude-oauth"},
		{Name: "ckitch@main.example", Type: "claude-oauth", Label: "Arena"},
		{Name: "ckitch@work.example", Type: "claude-oauth", Label: "work"},
		{Name: "ckitch@work2.example", Type: "claude-oauth", Label: "work-backup"},
	} {
		if err := config.UpsertAccount(path, a); err != nil {
			t.Fatalf("seed account %q: %v", a.Name, err)
		}
	}
	return path
}

// TestResolveConfigAccountName covers the resolver `accounts priority` and
// `accounts overage` both call: full resolution (name, label,
// prefix/substring), no daemon, unknown is still an error — neither of
// those two commands adds anything, so an unrecognised name must refuse
// exactly like `switch` does.
func TestResolveConfigAccountName(t *testing.T) {
	cfgPath := configFixture(t)
	for _, tc := range []struct {
		name  string
		query string
		want  string // "" means an error is expected
	}{
		{"exact name", "kimi", "kimi"},
		{"exact label", "work", "ckitch@work.example"},
		{"prefix/substring", "kimi-sta", "kimi-standby"},
		{"differing case label", "arena", "ckitch@main.example"},
		{"ambiguous", "wor", ""},
		{"unknown is an error, not a new account", "nope", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveConfigAccountName(cfgPath, tc.query)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("resolveConfigAccountName(%q) = %q, nil; want an error", tc.query, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveConfigAccountName(%q) unexpected error: %v", tc.query, err)
			}
			if got != tc.want {
				t.Errorf("resolveConfigAccountName(%q) = %q, want %q", tc.query, got, tc.want)
			}
		})
	}
}

// TestResolveLoginAccountNameResolvesExisting covers login's first policy
// call (#44): a name or label that matches an existing account resolves to
// it, same as every other command — this is what fixes the bug the issue was
// filed over (`login claude arena` reaching the duplicate-UUID guard with
// the literal string "arena" instead of the account's real name).
func TestResolveLoginAccountNameResolvesExisting(t *testing.T) {
	cfgPath := configFixture(t)
	for _, tc := range []struct {
		name  string
		query string
		want  string
	}{
		{"exact name", "kimi", "kimi"},
		{"exact label", "work", "ckitch@work.example"},
		{"prefix/substring", "kimi-sta", "kimi-standby"},
		{"differing case label", "arena", "ckitch@main.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveLoginAccountName(cfgPath, tc.query)
			if err != nil {
				t.Fatalf("resolveLoginAccountName(%q) unexpected error: %v", tc.query, err)
			}
			if got != tc.want {
				t.Errorf("resolveLoginAccountName(%q) = %q, want %q", tc.query, got, tc.want)
			}
		})
	}
}

// TestResolveLoginAccountNameUnknownIsANewAccount covers login's defining
// difference from every other command (#44): a name that matches nothing is
// not a refusal, it is how an account gets added. Same for an empty config
// (first login ever) — and that must not create the config file either,
// since resolveLoginAccountName runs before the OAuth exchange has even
// started.
func TestResolveLoginAccountNameUnknownIsANewAccount(t *testing.T) {
	cfgPath := configFixture(t)
	got, err := resolveLoginAccountName(cfgPath, "brand-new-name")
	if err != nil {
		t.Fatalf("unknown name must not error for login, got: %v", err)
	}
	if got != "brand-new-name" {
		t.Errorf("resolveLoginAccountName(%q) = %q, want the literal query back", "brand-new-name", got)
	}

	// No config file at all yet — the state the very first `login` runs in.
	freshPath := filepath.Join(t.TempDir(), "spillway.yaml")
	got, err = resolveLoginAccountName(freshPath, "first-account")
	if err != nil {
		t.Fatalf("first login (no config yet) must not error, got: %v", err)
	}
	if got != "first-account" {
		t.Errorf("resolveLoginAccountName(%q) = %q, want the literal query back", "first-account", got)
	}
	if _, err := os.Stat(freshPath); err == nil {
		t.Errorf("resolveLoginAccountName must not create the config file just to resolve a name")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat %s: %v", freshPath, err)
	}
}

// TestResolveLoginAccountNameAmbiguousStillRefuses covers the one case
// login does NOT fall back to "new account" for: ambiguity. Picking one of
// several candidates, or silently adding a near-duplicate, is worse than
// making the caller be more specific.
func TestResolveLoginAccountNameAmbiguousStillRefuses(t *testing.T) {
	cfgPath := configFixture(t)
	got, err := resolveLoginAccountName(cfgPath, "wor")
	if err == nil {
		t.Fatalf("resolveLoginAccountName(%q) = %q, nil; ambiguous input must still refuse", "wor", got)
	}
	if got != "" {
		t.Errorf("resolveLoginAccountName(%q) returned %q alongside an error", "wor", got)
	}
	msg := err.Error()
	for _, want := range []string{"ckitch@work.example", "ckitch@work2.example"} {
		if !strings.Contains(msg, want) {
			t.Errorf("ambiguity error does not name %q:\n%s", want, msg)
		}
	}
}

// TestResolveRemoveAccountName covers #44's second policy call: `accounts
// remove` resolves only an exact name or exact label, never the
// prefix/substring fallback tier the other three commands get — a fuzzy
// match there would delete the wrong credential, so it is refused rather
// than resolved.
func TestResolveRemoveAccountName(t *testing.T) {
	cfgPath := configFixture(t)
	for _, tc := range []struct {
		name  string
		query string
		want  string // "" means an error is expected
	}{
		{"exact name resolves", "kimi", "kimi"},
		{"exact label resolves", "work", "ckitch@work.example"},
		// "kimi-sta" and "standby" would resolve through switch/login/
		// priority/overage's fallback tier; remove must refuse both.
		{"prefix is refused, not resolved", "kimi-sta", ""},
		{"substring is refused, not resolved", "standby", ""},
		{"differing-case label is refused (label match is exact-case)", "arena", ""},
		{"unknown is an error", "nope", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveRemoveAccountName(cfgPath, tc.query)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("resolveRemoveAccountName(%q) = %q, nil; want an error", tc.query, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveRemoveAccountName(%q) unexpected error: %v", tc.query, err)
			}
			if got != tc.want {
				t.Errorf("resolveRemoveAccountName(%q) = %q, want %q", tc.query, got, tc.want)
			}
		})
	}
}
