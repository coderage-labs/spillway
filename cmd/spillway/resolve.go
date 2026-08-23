package main

// Name resolution, shared by every command that takes an account name (#44).
//
// #23 built this for `spillway switch` alone: exact name; exact label; then
// a unique case-insensitive prefix or substring, with ambiguity refusing
// rather than guessing. `login`, `accounts remove`, `accounts priority` and
// `accounts overage` took the exact string only — exactly where someone
// types `arena` instead of `ckitch@arenaentertainment.com` — until #44 wired
// the same resolver into them.
//
// The daemon's /api/pin, and the config functions in internal/config,
// match `name` exactly — resolution belongs at the edge, where a human's
// shorthand is, not in the selector or the store. This is that edge: it
// turns what a person typed into a real account name before anything else
// sees it.
//
// resolveAccountName itself stays a pure function over a slice, with no
// daemon and no config file in sight. Two sources feed it:
//   - fetchSwitchAccounts (switch.go) reads /api/accounts, for `switch`,
//     which needs the daemon anyway.
//   - configAccounts (below) reads the config file directly, for the other
//     four commands, which must keep working with the daemon stopped.
//
// Keeping the split mirrors the same switchArgs/parseSwitchArgs one #23
// already drew, and for the same reason: a test that has to go through the
// daemon, or write a real config file, to reach this logic only ever
// exercises the connection failure.

import (
	"fmt"
	"strings"

	"github.com/coderage-labs/spillway/internal/config"
)

// switchAccount is the subset of one /api/accounts entry the resolver and
// the bare report need.
type switchAccount struct {
	Name          string
	Label         string
	State         string
	Paid          bool
	OverThreshold bool
}

// resolveAccountName turns query into a real account name, matching in
// order: exact name; exact label; then a unique case-insensitive prefix or
// substring of either (prefix is just a substring that happens to start at
// 0, so one check covers both). Ambiguous input is an error naming every
// candidate — it never picks one. Unknown input is an error listing what
// exists.
func resolveAccountName(accounts []switchAccount, query string) (string, error) {
	if strings.TrimSpace(query) == "" {
		return "", fmt.Errorf("no account given")
	}
	if len(accounts) == 0 {
		return "", fmt.Errorf("no accounts configured")
	}

	for _, a := range accounts {
		if a.Name == query {
			return a.Name, nil
		}
	}
	for _, a := range accounts {
		if a.Label != "" && a.Label == query {
			return a.Name, nil
		}
	}

	q := strings.ToLower(query)
	seen := map[string]bool{}
	var matched []switchAccount
	for _, a := range accounts {
		if caseInsensitiveContains(a.Name, q) || caseInsensitiveContains(a.Label, q) {
			if !seen[a.Name] {
				seen[a.Name] = true
				matched = append(matched, a)
			}
		}
	}

	switch len(matched) {
	case 0:
		return "", fmt.Errorf("no account matches %q — have: %s", query, describeAccounts(accounts))
	case 1:
		return matched[0].Name, nil
	default:
		return "", &ambiguousAccountError{query: query, candidates: matched}
	}
}

// ambiguousAccountError is resolveAccountName's ambiguous-match failure,
// kept as its own type rather than a plain fmt.Errorf so login can tell it
// apart from every other failure (unknown, empty, no accounts). For login
// alone, those other failures mean "this is a new account" — ambiguity is
// the one outcome that must still refuse rather than guess or add.
type ambiguousAccountError struct {
	query      string
	candidates []switchAccount
}

func (e *ambiguousAccountError) Error() string {
	return fmt.Sprintf("%q is ambiguous between %s", e.query, describeAccounts(e.candidates))
}

// caseInsensitiveContains reports whether s contains q, folding case. An
// empty s (an account with no label) never matches — otherwise every
// account without a label would "contain" any query once both sides were
// lowercased to "".
func caseInsensitiveContains(s, lowerQuery string) bool {
	if s == "" {
		return false
	}
	return strings.Contains(strings.ToLower(s), lowerQuery)
}

// describeAccounts renders a comma-separated list for an error or report —
// label and name together when they differ, so the reader can see both what
// they might type and what it resolves to.
func describeAccounts(accounts []switchAccount) string {
	parts := make([]string, len(accounts))
	for i, a := range accounts {
		parts[i] = describeAccount(a)
	}
	return strings.Join(parts, ", ")
}

// describeAccount is one account's short human-readable form.
func describeAccount(a switchAccount) string {
	if a.Label != "" && a.Label != a.Name {
		return fmt.Sprintf("%s (%s)", a.Label, a.Name)
	}
	return a.Name
}

// configAccounts reads the accounts recorded in the config file at path,
// with no daemon involved — the source resolveAccountName needs for
// `login`, `accounts remove`, `accounts priority` and `accounts overage`,
// all of which must keep working while the daemon is stopped. It uses
// config.ListAccountConfigs rather than config.LoadFrom deliberately: a name
// lookup has no business creating the config file when it doesn't exist yet
// (the state a first `login` runs in, before its OAuth exchange has even
// completed).
func configAccounts(cfgPath string) ([]switchAccount, error) {
	accts, err := config.ListAccountConfigs(cfgPath)
	if err != nil {
		return nil, err
	}
	out := make([]switchAccount, len(accts))
	for i, a := range accts {
		out[i] = switchAccount{Name: a.Name, Label: a.Label}
	}
	return out, nil
}

// resolveConfigAccountName resolves query against the config file at
// cfgPath — full resolution (exact name, exact label, unique
// prefix/substring), no daemon required. Shared by `accounts priority` and
// `accounts overage`: both mutate an existing account's metadata, so an
// unknown name is exactly as much an error here as it is for `switch`.
func resolveConfigAccountName(cfgPath, query string) (string, error) {
	accts, err := configAccounts(cfgPath)
	if err != nil {
		return "", err
	}
	return resolveAccountName(accts, query)
}

// resolveLoginAccountName decides what `login claude|kimi <name>` should
// treat name as (#44's first policy call).
//
// login is the one command that legitimately takes a name that does not
// exist yet — that is how an account gets added — so "no match" cannot be
// an error here the way it is everywhere else: it means "add a new account
// called exactly this". Only ambiguity still refuses, because picking one
// of several candidates (or silently adding a near-duplicate) is worse than
// making the caller be more specific.
//
// This is also what fixes the bug #44 was filed over: `login claude arena`
// used to reach the duplicate-UUID guard with the literal string "arena" as
// the candidate new name, which the guard correctly refused since "arena"
// never matched the disabled account's real name. Resolving first turns
// "arena" into that account's real name before the guard ever sees it, so
// re-authenticating it goes through instead.
func resolveLoginAccountName(cfgPath, query string) (string, error) {
	accts, err := configAccounts(cfgPath)
	if err != nil {
		return "", err
	}
	resolved, err := resolveAccountName(accts, query)
	if err == nil {
		return resolved, nil
	}
	if _, ambiguous := err.(*ambiguousAccountError); ambiguous {
		return "", err
	}
	// Unknown, empty accounts, or (in principle) an empty query: none of
	// these are refusals for login. Fall back to the literal argument as a
	// new account name, same as before this resolver existed.
	return query, nil
}

// resolveRemoveAccountName resolves query for `accounts remove` (#44's
// second policy call): exact name or exact label only, never the
// prefix/substring fallback tier.
//
// remove is destructive — a fuzzy match resolving to the wrong account
// deletes the wrong credential, with nothing left to undo it. Exact name
// and exact label are both things a caller can read back off `spillway
// accounts` and know for certain they are deleting the right thing; a
// substring match is a guess by construction, and a guess is the wrong
// failure mode to make convenient on a delete. So this refuses the fallback
// tier entirely rather than resolving-and-confirming: no prompt to script
// around, no state where `remove` silently did something other than what
// was typed.
func resolveRemoveAccountName(cfgPath, query string) (string, error) {
	accts, err := configAccounts(cfgPath)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(query) == "" {
		return "", fmt.Errorf("no account given")
	}
	if len(accts) == 0 {
		return "", fmt.Errorf("no accounts configured")
	}
	for _, a := range accts {
		if a.Name == query {
			return a.Name, nil
		}
	}
	for _, a := range accts {
		if a.Label != "" && a.Label == query {
			return a.Name, nil
		}
	}
	return "", fmt.Errorf(
		"%q is not an existing account's exact name or label — have: %s\n"+
			"  accounts remove only takes an exact match; run `spillway accounts` to see the exact names",
		query, describeAccounts(accts))
}
