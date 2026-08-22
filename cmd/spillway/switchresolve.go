package main

// Name resolution for `spillway switch` (#23).
//
// The daemon's /api/pin matches `name` exactly — internal/pool's Pin keeps
// doing that on purpose, because resolution belongs at the edge, where a
// human's shorthand is, not in the selector. This is that edge: it turns
// what a person typed into a real account name before the CLI ever calls
// the daemon.
//
// Deliberately a pure function over a slice, with no daemon and no adminAPI
// in sight. runSwitch does the fetching (fetchSwitchAccounts, in switch.go)
// and hands the result here — the same split switchArgs/parseSwitchArgs
// already draws, and for the same reason: a test that has to go through the
// daemon to reach this logic only ever exercises the connection failure.

import (
	"fmt"
	"strings"
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
		return "", fmt.Errorf("%q is ambiguous between %s", query, describeAccounts(matched))
	}
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
