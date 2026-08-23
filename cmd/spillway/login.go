package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/coderage-labs/spillway/internal/accounts"
	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/provider"
	"github.com/coderage-labs/spillway/internal/secrets"
)

// runLoginClaude implements `spillway login claude <name>`: PKCE auth-code
// flow with manual paste-back (the official CLI's code#state format), tokens
// to the keychain, metadata to the yaml.
func runLoginClaude(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: spillway login claude <name>")
	}
	cfgPath, err := config.Path()
	if err != nil {
		return err
	}
	// Resolve before anything else — in particular before the OAuth round
	// trip below — so an ambiguous query is refused immediately rather than
	// after the browser comes back. Unknown input is not an error here: it
	// is a new account name, resolved to itself (#44).
	name, err := resolveLoginAccountName(cfgPath, args[0])
	if err != nil {
		return err
	}

	pkce, err := accounts.GeneratePKCE()
	if err != nil {
		return err
	}
	// Prefer the loopback callback: the browser hands the code back itself,
	// so there is nothing to copy and nothing to truncate. Falls back to
	// paste whenever the port cannot be had — over SSH, or with a second
	// login already in progress — which is also the only mode that works
	// with no browser at all.
	code, redirectURI, err := authorizeClaude(pkce)
	if err != nil {
		return err
	}

	fmt.Println("Exchanging authorization code for tokens...")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The exchange must repeat the redirect_uri from the authorize request:
	// the provider compares them, and they differ between the two modes.
	tokens, err := accounts.ExchangeCode(ctx, nil, "", code, pkce, redirectURI)
	if err != nil {
		return err
	}

	profile, err := accounts.FetchProfile(ctx, nil, "", tokens.AccessToken)
	if err != nil {
		// Profile failure is not fatal: tokens work without a uuid (the
		// rewrite just stays off) — but say so.
		fmt.Fprintf(os.Stderr, "warning: profile fetch failed (%v) — account_uuid rewrite disabled for %s\n", err, name)
		profile = &accounts.Profile{}
	}

	// Refuse a duplicate before the secret is written, not after. Upsert
	// rejects it either way, but writing first leaves token material in the
	// keychain under a name no config will ever reference again.
	if dup, derr := config.FindAccountByUUID(cfgPath, profile.AccountUUID); derr == nil && dup != "" && dup != name {
		return fmt.Errorf("that is the same provider account as %q (account uuid %s)\n"+
			"  to re-authenticate it:  spillway login claude %s\n"+
			"  to replace it:          spillway accounts remove %s",
			dup, profile.AccountUUID, dup, dup)
	}

	store := openSecrets()
	if err := store.Set(name, secrets.Secrets{
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
	}); err != nil {
		return err
	}
	if err := config.UpsertAccount(cfgPath, config.AccountConfig{
		Name:        name,
		Type:        "claude-oauth",
		ExpiresAt:   tokens.ExpiresAt,
		AccountUUID: profile.AccountUUID,
	}); err != nil {
		return err
	}
	fmt.Printf("logged in: %s (%s, org %s) — token expires %s\n",
		name, profile.Email, profile.OrgName,
		time.UnixMilli(tokens.ExpiresAt).UTC().Format(time.RFC3339))
	return nil
}

// runLoginKimi implements `spillway login kimi <name>`: RFC 8628 device
// flow (design doc §12a), tokens to the keychain, metadata to the yaml.
func runLoginKimi(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: spillway login kimi <name>")
	}
	cfgPath, err := config.Path()
	if err != nil {
		return err
	}
	name, err := resolveLoginAccountName(cfgPath, args[0])
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	da, err := provider.KimiDeviceAuthorize(ctx, nil, "")
	if err != nil {
		return err
	}
	fmt.Printf("Approve the login at:\n  %s\nuser code: %s\n", da.VerificationURIComplete, da.UserCode)
	openBrowser(da.VerificationURIComplete)
	fmt.Println("Waiting for approval...")

	tokens, err := provider.KimiPollDevice(ctx, nil, "", da)
	if err != nil {
		return err
	}

	store := openSecrets()
	if err := store.Set(name, secrets.Secrets{
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
	}); err != nil {
		return err
	}
	if err := config.UpsertAccount(cfgPath, config.AccountConfig{
		Name:      name,
		Type:      "kimi-oauth",
		Upstream:  provider.KimiUpstream,
		ExpiresAt: tokens.ExpiresAtMs(time.Now()),
	}); err != nil {
		return err
	}
	fmt.Printf("logged in: %s (kimi) — token expires %s\n",
		name, time.UnixMilli(tokens.ExpiresAtMs(time.Now())).UTC().Format(time.RFC3339))
	fmt.Println("note: set modelMap for this account in the config, e.g. claude-sonnet-4-6 → your kimi model id (see README)")
	return nil
}

// accountRow is one line of `spillway accounts` output.
type accountRow struct {
	Name      string
	Type      string
	Source    string
	ExpiresAt int64
	UUID      string
	Secrets   string // "present" / "missing" / "keychain"
	Status    string // ok / expired / no-secrets
	// Overage is the account's allowOverage setting: nil follows the pool.
	// Shown in the listing so the one setting that spends money is visible
	// without opening the config.
	Overage *bool
	// Priority orders selection; lower is preferred. Listed because equal
	// priorities are what let a transient load blip decide which provider a
	// session spends its life on.
	Priority int
}

// listAccounts builds the listing from config metadata + the secret store.
// liveClaude reports the credential the claude CLI currently holds. Injected
// so tests need no keychain.
type liveClaude func(now time.Time) (*accounts.ClaudeOAuth, error)

func listAccounts(cfg *config.Config, store secrets.Store, live liveClaude, now time.Time) []accountRow {
	var rows []accountRow
	for _, a := range cfg.Accounts {
		row := accountRow{
			Name: a.Name, Type: a.Type, Source: a.Source,
			ExpiresAt: a.ExpiresAt, UUID: a.AccountUUID,
			Overage: a.AllowOverage, Priority: a.Priority,
		}
		if a.Source == "keychain" {
			row.Secrets = "keychain"
			row.Status = "ok"
			// The CLI owns this credential and refreshes it on its own
			// schedule, so the expiry recorded in our yaml is a snapshot that
			// goes stale immediately. Read the live one, or this reports
			// "expired" for a perfectly healthy account.
			if live != nil {
				if o, err := live(now); err == nil {
					row.ExpiresAt = o.ExpiresAt
				} else {
					row.Secrets = "missing"
					row.Status = "no-secrets"
				}
			}
		} else {
			if _, err := store.Get(a.Name); err != nil {
				row.Secrets = "missing"
				row.Status = "no-secrets"
			} else {
				row.Secrets = "present"
				row.Status = "ok"
			}
		}
		if row.Status == "ok" && row.ExpiresAt > 0 && row.ExpiresAt <= now.UnixMilli() {
			row.Status = "expired"
		}
		rows = append(rows, row)
	}
	return rows
}

// defaultLiveClaude reads the claude CLI's own login from the keychain.
func defaultLiveClaude(now time.Time) (*accounts.ClaudeOAuth, error) {
	return accounts.LoadClaude(accounts.DefaultSource(), now)
}

// runAccounts implements `spillway accounts [remove <name>]`.
func runAccounts(args []string) error {
	cfgPath, err := config.Path()
	if err != nil {
		return err
	}
	store := openSecrets()

	if len(args) >= 2 && args[0] == "remove" {
		// Exact name or exact label only (#44) — remove is destructive, so
		// the prefix/substring fallback tier resolveAccountName otherwise
		// offers is refused rather than resolved. See resolveRemoveAccountName.
		name, err := resolveRemoveAccountName(cfgPath, args[1])
		if err != nil {
			return err
		}
		if err := config.RemoveAccount(cfgPath, name); err != nil {
			return err
		}
		if err := store.Delete(name); err != nil {
			return err
		}
		fmt.Println("removed:", name)
		return nil
	}
	if len(args) >= 2 && args[0] == "overage" {
		name, err := resolveConfigAccountName(cfgPath, args[1])
		if err != nil {
			return err
		}
		return setOverage(cfgPath, append([]string{name}, args[2:]...))
	}
	if len(args) >= 2 && args[0] == "priority" {
		name, err := resolveConfigAccountName(cfgPath, args[1])
		if err != nil {
			return err
		}
		return setPriority(cfgPath, append([]string{name}, args[2:]...))
	}
	if len(args) > 0 {
		return fmt.Errorf("usage: spillway accounts [remove <name>] " +
			"[overage <name> on|off|default] [priority <name> <n>]")
	}

	cfg, err := config.LoadFrom(cfgPath)
	if err != nil {
		return err
	}
	rows := listAccounts(cfg, store, defaultLiveClaude, time.Now())
	if len(rows) == 0 {
		fmt.Println("no accounts — run `spillway login claude <name>`")
		return nil
	}
	t := newTable("account", "type", "status", "secrets", "expires", "priority", "extra usage", "uuid")
	t.rightAlign(5)
	for _, r := range rows {
		expiry := "never"
		if r.ExpiresAt > 0 {
			expiry = time.UnixMilli(r.ExpiresAt).UTC().Format(time.RFC3339)
		}
		uuid := r.UUID
		if uuid == "" {
			uuid = "—"
		}
		// Three states, not two: unset means "follow pool.allowOverage", and
		// an explicit false overrides a pool-wide yes.
		overage := "pool default"
		if r.Overage != nil {
			overage = "off"
			if *r.Overage {
				overage = "ON (billable)"
			}
		}
		t.add(r.Name, r.Type, r.Status, r.Secrets, expiry, strconv.Itoa(r.Priority), overage, uuid)
	}
	t.render(os.Stdout)
	return nil
}

func readLine() string {
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(line)
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		// rundll32 rather than `cmd /c start`: start is a shell builtin whose
		// first quoted argument is taken as a window title, and an OAuth URL
		// is full of characters cmd.exe would rather interpret than pass on.
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start() // printed URL is the fallback
}

// authorizeClaude drives the browser half of the flow and returns the code
// plus the redirect_uri it was obtained with.
func authorizeClaude(pkce *accounts.PKCE) (code, redirectURI string, err error) {
	cs, lerr := accounts.StartCallback(pkce.State)
	if lerr != nil {
		// Not an error worth stopping for: paste always works.
		fmt.Fprintf(os.Stderr, "note: %v — falling back to copy/paste\n", lerr)
		return pasteFlow(pkce)
	}
	defer cs.Close()

	authURL := accounts.AuthorizeURL(cs.RedirectURI(), pkce)
	fmt.Println("Opening browser for authentication...")
	fmt.Printf("If it doesn't open, visit:\n  %s\n\n", authURL)
	openBrowser(authURL)
	fmt.Println("Waiting for the browser to come back (Ctrl-C to cancel)...")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	code, err = cs.Wait(ctx, 5*time.Minute)
	if err != nil {
		return "", "", err
	}
	return code, cs.RedirectURI(), nil
}

// pasteFlow is the headless path: print the URL, read `code#state` back.
func pasteFlow(pkce *accounts.PKCE) (code, redirectURI string, err error) {
	authURL := accounts.AuthorizeURL(accounts.ManualRedirectURI, pkce)
	fmt.Printf("Visit:\n  %s\n\n", authURL)
	openBrowser(authURL)
	fmt.Print("Paste authorization code (code#state): ")
	code, err = accounts.ParseCode(readLine(), pkce.State)
	if err != nil {
		return "", "", err
	}
	return code, accounts.ManualRedirectURI, nil
}

// setOverage implements `spillway accounts overage <name> on|off|default`.
//
// A command rather than a dashboard toggle: every other setting there changes
// how long a request waits, and this one decides whether it is charged.
func setOverage(cfgPath string, args []string) error {
	name := args[0]
	if len(args) < 2 {
		return fmt.Errorf("usage: spillway accounts overage %s on|off|default", name)
	}
	var v *bool
	switch args[1] {
	case "on", "true", "yes":
		t := true
		v = &t
	case "off", "false", "no":
		f := false
		v = &f
	case "default", "unset", "":
		v = nil // follow pool.allowOverage
	default:
		return fmt.Errorf("overage must be on, off or default (got %q)", args[1])
	}
	if err := config.SetAccountOverage(cfgPath, name, v); err != nil {
		return err
	}
	switch {
	case v == nil:
		fmt.Printf("%s: extra usage follows pool.allowOverage\n", name)
	case *v:
		// Say what was just agreed to, in the words of a bill.
		fmt.Printf("%s: extra usage ENABLED — once this account's subscription quota is\n"+
			"  gone, spillway may keep serving from it at pay-as-you-go rates and you\n"+
			"  will be charged. It is still the last tier tried, after every free\n"+
			"  account is spent. Turn it off with:\n"+
			"    spillway accounts overage %s off\n", name, name)
	default:
		fmt.Printf("%s: extra usage disabled — this account will never be billed,\n"+
			"  even if pool.allowOverage is true\n", name)
	}
	fmt.Println("restart the daemon for this to take effect")
	return nil
}

// setPriority implements `spillway accounts priority <name> <n>`.
//
// Priority is what stops the pool treating unlike accounts as peers. Below
// the rotate-away threshold the selector breaks ties on in-flight count
// alone, so a stray request landing on the wrong account at the wrong moment
// is enough to pick the other one — and the session then stays there, on that
// provider, for its whole life (§6.18). Ranking them makes that decision
// deliberate rather than a race.
func setPriority(cfgPath string, args []string) error {
	name := args[0]
	if len(args) < 2 {
		return fmt.Errorf("usage: spillway accounts priority %s <n>", name)
	}
	prio, err := strconv.Atoi(args[1])
	if err != nil {
		return fmt.Errorf("priority must be a whole number (got %q)", args[1])
	}
	if err := config.SetAccountPriority(cfgPath, name, prio); err != nil {
		return err
	}
	fmt.Printf("%s: priority %d — lower is preferred, and an account is only\n"+
		"  reached for when everything above it cannot serve\n", name, prio)
	fmt.Println("restart the daemon for this to take effect, or set it in the dashboard,")
	fmt.Println("which applies immediately")
	return nil
}
