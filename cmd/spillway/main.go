package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/coderage-labs/spillway/internal/accounts"
	"github.com/coderage-labs/spillway/internal/admin"
	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/events"
	"github.com/coderage-labs/spillway/internal/mitm"
	"github.com/coderage-labs/spillway/internal/pool"
	"github.com/coderage-labs/spillway/internal/proxy"
	"github.com/coderage-labs/spillway/internal/reqlog"
	"github.com/coderage-labs/spillway/internal/secrets"
)

// refreshSweepInterval is how often credentials are checked against their
// expiry. Well under accounts.refreshWindow (5m) so a token cannot slip past
// it between sweeps.
const refreshSweepInterval = time.Minute

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "server":
		err = runServer()
	case "run":
		err = runClaude(os.Args[2:])
	case "status":
		err = runStatus(len(os.Args) > 2 && os.Args[2] == "--json")
	case "login":
		if len(os.Args) < 3 || (os.Args[2] != "claude" && os.Args[2] != "kimi") {
			usage()
			os.Exit(2)
		}
		if os.Args[2] == "claude" {
			err = runLoginClaude(os.Args[3:])
		} else {
			err = runLoginKimi(os.Args[3:])
		}
	case "accounts":
		err = runAccounts(os.Args[2:])
	case "statusline":
		err = runStatusline(os.Args[2:])
	case "service":
		err = runService(os.Args[2:])
	case "version", "--version", "-v":
		err = runVersion()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		var exitErr *ExitCodeError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.Code)
		}
		fmt.Fprintln(os.Stderr, "spillway:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, strings.Join([]string{
		"usage: spillway server",
		"       spillway run [-- <claude args>]",
		"       spillway status [--json]",
		"       spillway login claude <name>",
		"       spillway login kimi <name>",
		"       spillway accounts [remove <name>]",
		"       spillway accounts overage <name> on|off|default   allow pay-as-you-go past quota",
		"       spillway statusline                          print the status line (for Claude Code)",
		"       spillway statusline [install|uninstall|status]",
		"       spillway service [install|uninstall|status]  run the daemon under launchd",
		"       spillway version",
	}, "\n"))
}

func runServer() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: parseLevel(cfg.Log.Level),
	}))

	store := secrets.NewKeyring()
	// Scrub any inline tokens from an older config into the keychain (§5).
	cfgPath, err := config.Path()
	if err != nil {
		return err
	}
	if migrated, err := config.MigrateInlineSecrets(cfgPath, store); err != nil {
		logger.Warn("secret migration failed", "err", err)
	} else {
		for _, name := range migrated {
			logger.Info("migrated account secrets to keychain", "name", name)
		}
	}
	// Migration may have scrubbed tokens from the file — reload.
	if cfg, err = config.Load(); err != nil {
		return err
	}

	p, err := buildPool(cfg, store, logger, time.Now())
	if err != nil {
		return err
	}

	// Request log + event broker + admin listener (§5: separate trust
	// class from the proxy port).
	rl, err := reqlog.Open(filepath.Join(filepath.Dir(cfgPath), "spillway-requests.db"))
	if err != nil {
		return fmt.Errorf("open request log: %w", err)
	}
	defer rl.Close()
	broker := events.New()

	handler, err := proxy.NewHandler(cfg, logger, p)
	if err != nil {
		return err
	}
	handler.SetHooks(proxy.Hooks{Log: rl, Events: broker})

	// Keep credentials fresh regardless of traffic. EnsureFresh is a no-op
	// until a token is within its refresh window, so this sweep is almost
	// always a comparison; without it an account with a current quota reading
	// but an expiring token has nothing to trigger a refresh, and only finds
	// out when something finally routes to it.
	go func() {
		t := time.NewTicker(refreshSweepInterval)
		defer t.Stop()
		for range t.C {
			for _, a := range p.Accounts() {
				if a.State() == pool.StateDisabled {
					continue
				}
				if err := p.EnsureFresh(context.Background(), a); err != nil {
					logger.Warn("scheduled refresh failed", "account", a.Name, "err", err)
				}
			}
		}
	}()

	// One cheap probe per idle account so its tank shows a level immediately
	// rather than "awaiting signal" until the pool happens to route to it.
	if cfg.Pool.ProbeOnStart == nil || *cfg.Pool.ProbeOnStart {
		probeEvery := cfg.PoolProbeInterval()
		go func() {
			client := &http.Client{Timeout: 30 * time.Second}
			// Startup: fill anything with no reading at all.
			accounts.ProbeIdle(context.Background(), p, client, cfg.Upstream, 0, logger)
			if probeEvery <= 0 {
				return
			}
			// Then keep standby accounts current. An account that is serving
			// gets fresh headers for free and is skipped by the staleness
			// check, so this costs one request per idle account per tick.
			t := time.NewTicker(probeEvery)
			defer t.Stop()
			for range t.C {
				accounts.ProbeIdle(context.Background(), p, client, cfg.Upstream, probeEvery, logger)
			}
		}()
	}

	// Canary: watch for credentials that die without any request of yours
	// failing first (§6.8b). Shares the probe's request, and only for
	// accounts that have gone unused.
	if cfg.Pool.CanaryInterval != "0" {
		every := cfg.PoolCanaryInterval()
		go func() {
			c := &accounts.Canary{
				Pool: p, Client: &http.Client{Timeout: 30 * time.Second},
				Upstream: cfg.Upstream, Logger: logger, Events: broker,
				Idle: every,
			}
			t := time.NewTicker(every)
			defer t.Stop()
			for range t.C {
				c.Run(context.Background())
			}
		}()
	}

	// Snapshot quota into the log on a timer. One provider-agnostic writer
	// beats hooking every producer: headroom-over-time is the only thing the
	// dashboard can't derive from live state, and a burn rate needs a past.
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			for _, a := range p.Accounts() {
				for _, w := range a.QuotaWindows() {
					if err := rl.RecordQuota(reqlog.Sample{
						Ts: time.Now(), Account: a.Name, Window: w.Name,
						Limit: w.Limit, Used: w.Used, ResetAt: w.ResetAt,
					}); err != nil {
						logger.Debug("quota sample failed", "account", a.Name, "err", err)
					}
				}
			}
			<-t.C
		}
	}()

	adminAddr := cfg.Admin.Addr
	if adminAddr == "" {
		adminAddr = admin.DefaultAddr
	}
	tokenPath := filepath.Join(filepath.Dir(cfgPath), "spillway-admin-token")

	// Loopback needs no token: it would only stop processes running as
	// another user, at the cost of a secret in the URL and a dead browser tab
	// on every restart. Binding anywhere else makes one mandatory.
	token := cfg.Admin.Token
	// A unix socket is loopback by construction — it has no network presence
	// at all, and its file mode is the access control.
	loopback := admin.IsLoopback(adminAddr) || admin.IsUnix(adminAddr)
	if !loopback && token == "" {
		token, err = admin.GenerateToken()
		if err != nil {
			return err
		}
	}
	adminSrv := &http.Server{Handler: nil}
	adminHandler := admin.New(p, rl, broker, token)
	if !loopback {
		adminHandler.RequireToken()
		if err := admin.WriteTokenFile(tokenPath, token); err != nil {
			return fmt.Errorf("write admin token: %w", err)
		}
	} else if err := os.Remove(tokenPath); err != nil && !os.IsNotExist(err) {
		// A token left from an earlier non-loopback run would be misleading.
		logger.Warn("could not remove stale admin token file", "path", tokenPath, "err", err)
	}
	// Live-apply the settings a dashboard write changes, so an edit does not
	// need a restart (which would drop the SSE stream and re-probe).
	adminHandler.EnableSettings(cfgPath, func(nc *config.Config) {
		p.SwitchThreshold = nc.Pool.SwitchThreshold
		p.CrossProvider = nc.Pool.CrossProvider
		p.AllowOverage = nc.Pool.AllowOverage
		disabled := map[string]bool{}
		for _, a := range nc.Accounts {
			disabled[a.Name] = a.Disabled
		}
		for _, a := range p.Accounts() {
			// Park/Unpark, not Disable/Enable: un-parking must never revive
			// an account whose credential died.
			if disabled[a.Name] {
				a.Park()
			} else {
				a.Unpark()
			}
			for _, ca := range nc.Accounts {
				if ca.Name == a.Name {
					a.Label = ca.Label
					a.Priority = ca.Priority
					a.AllowOverage = ca.AllowOverage
				}
			}
		}
		logger.Info("settings updated from dashboard",
			"switchThreshold", nc.Pool.SwitchThreshold, "crossProvider", nc.Pool.CrossProvider)
	})
	adminSrv.Handler = adminHandler
	// A unix socket is the tightest option (§5): file permissions replace
	// the token entirely, and nothing on the network can reach it at all.
	adminLn, adminDesc, err := admin.Listen(adminAddr)
	if err != nil {
		return fmt.Errorf("admin listener: %w", err)
	}
	go func() {
		switch {
		case admin.IsUnix(adminAddr):
			logger.Info("admin listener", "socket", adminDesc, "auth", "unix file permissions")
		case loopback:
			logger.Info("admin listener", "addr", adminDesc, "auth", "none (loopback)")
		default:
			logger.Warn("admin listener is NOT loopback — token required",
				"addr", adminDesc, "token_file", tokenPath)
		}
		if err := adminSrv.Serve(adminLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("admin listener died", "err", err)
		}
	}()

	// MITM mode: CA key lives in the keychain, cert pem next to the config.
	// Failure degrades to base-URL mode only — loudly.
	pemPath, err := caPEMPath()
	if err != nil {
		return err
	}
	if ca, err := mitm.EnsureCA(secrets.NewKeyring(), pemPath); err != nil {
		logger.Error("MITM CA unavailable — CONNECT termination disabled, base-URL mode still works", "err", err)
	} else {
		handler.SetMITM(ca)
		logger.Info("MITM CA ready", "pem", pemPath)
	}

	addr := net.JoinHostPort(cfg.Proxy.Host, strconv.Itoa(cfg.Proxy.Port))
	srv := &http.Server{Addr: addr, Handler: handler}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Kimi quota state comes from polling /usages — no rate-limit headers
	// exist to learn it from (§6.5).
	accounts.StartUsagesPoller(ctx, p, logger)

	go func() {
		<-ctx.Done()
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		_ = adminSrv.Shutdown(shutdownCtx)
	}()

	logger.Info("spillway listening", "addr", "http://"+addr,
		"upstream", cfg.Upstream, "build", buildInfo())
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// buildPool resolves configured accounts (keychain-sourced, or metadata in
// yaml + tokens in the secret store) into a pool and wires credential
// recovery. With no accounts configured it falls back to the local claude
// keychain login as a single account. Startup fails when nothing is usable.
func buildPool(cfg *config.Config, store secrets.Store, logger *slog.Logger, now time.Time) (*pool.Pool, error) {
	cfgPath, err := config.Path()
	if err != nil {
		return nil, err
	}
	mgr := accounts.NewManager(cfgPath, accounts.DefaultSource(), store, logger)

	var accts []*pool.Account
	for _, a := range cfg.Accounts {
		var acct *pool.Account
		if a.Source == "keychain" {
			oauth, err := accounts.LoadClaude(accounts.DefaultSource(), now)
			if err != nil {
				return nil, fmt.Errorf("account %q: %w", a.Name, err)
			}
			acct = pool.NewAccount(a.Name, pool.SourceKeychain,
				oauth.AccessToken, oauth.RefreshToken, oauth.ExpiresAt, a.Upstream)
			acct.Type = "claude-oauth"
			acct.Label = a.Label
			acct.Priority = a.Priority
			acct.AllowOverage = a.AllowOverage
			// Never log token material — subscription and scopes only.
			logger.Info("claude account loaded",
				"name", a.Name,
				"subscription", oauth.SubscriptionType,
				"scopes", strings.Join(oauth.Scopes, ","),
				"expires", time.UnixMilli(oauth.ExpiresAt).UTC().Format(time.RFC3339),
			)
		} else {
			acct, err = accounts.ResolveYAML(a, store)
			if err != nil {
				return nil, err
			}
			logger.Info("account loaded",
				"name", a.Name,
				"expires", time.UnixMilli(a.ExpiresAt).UTC().Format(time.RFC3339),
			)
		}
		accts = append(accts, acct)
	}
	if len(accts) == 0 {
		oauth, err := accounts.LoadClaude(accounts.DefaultSource(), now)
		if err != nil {
			return nil, err
		}
		logger.Info("claude account loaded",
			"name", "local",
			"subscription", oauth.SubscriptionType,
			"scopes", strings.Join(oauth.Scopes, ","),
			"expires", time.UnixMilli(oauth.ExpiresAt).UTC().Format(time.RFC3339),
		)
		accts = append(accts, pool.NewAccount("local", pool.SourceKeychain,
			oauth.AccessToken, oauth.RefreshToken, oauth.ExpiresAt, ""))
		accts[0].Type = "claude-oauth"
	}

	// Config-level disable: parked by the operator, distinct from an account
	// disabled because its credential died.
	byName := map[string]bool{}
	for _, a := range cfg.Accounts {
		byName[a.Name] = a.Disabled
	}
	for _, a := range accts {
		if byName[a.Name] {
			a.Park()
			logger.Info("account parked by config", "name", a.Name)
		}
	}

	p := pool.New(accts, now)
	p.SwitchThreshold = cfg.Pool.SwitchThreshold
	p.AllowOverage = cfg.Pool.AllowOverage
	p.SetTokenManager(mgr)
	p.CrossProvider = cfg.Pool.CrossProvider
	usable := 0
	for _, a := range accts {
		if a.State() != pool.StateDisabled {
			usable++
		} else {
			logger.Warn("account disabled: token expired and no refresh token", "name", a.Name)
		}
	}
	if usable == 0 {
		return nil, errors.New("no usable accounts: every token is expired and unrecoverable — run `spillway login claude <name>`")
	}
	return p, nil
}

func parseLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
