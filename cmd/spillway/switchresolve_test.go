package main

import (
	"strings"
	"testing"
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
