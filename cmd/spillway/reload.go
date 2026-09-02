package main

// Issue #84: reload the config when anything OTHER than spillway's own CLI
// edits it — a text editor, a script, a dotfile synced from another
// machine. #83 made the CLI's own mutations reach the running pool; this is
// the general case, and it subsumes the class: any writer of the yaml gets
// live behaviour without every future command having to remember to notify
// the daemon.
//
// The motivating report was `spillway notify set phone`, which wrote the
// config and the credential correctly and then did nothing, because
// buildNotifier ran once at startup. A restart was required for a feature
// whose entire purpose is telling someone something is wrong while they are
// away from the machine.
//
// ONE APPLICATION PATH. liveApplier.apply is the only place a config
// becomes running state, and both callers use it: the admin settings
// endpoint (the dashboard's PUT, and the POST the CLI sends after
// `accounts priority` / `accounts overage` / `notify set`) and the file
// watcher below. A second "apply" implementation for the watcher would
// drift from the first, and the matrix of what-is-live would become a lie.
//
// §5 is unchanged by any of this: the config holds metadata only. A reload
// reads names, providers, events, priorities and flags from the yaml, and
// every credential — an account's tokens under "account:<name>", a notify
// channel's destination under "notify:<channel>" — comes from the secret
// store. Nothing here writes a secret into the config, and nothing here
// logs a destination, topic, token or credential; log lines carry account
// and channel NAMES, which are already in the file.

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coderage-labs/spillway/internal/accounts"
	"github.com/coderage-labs/spillway/internal/admin"
	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/notify"
	"github.com/coderage-labs/spillway/internal/pool"
	"github.com/coderage-labs/spillway/internal/provider"
	"github.com/coderage-labs/spillway/internal/secrets"
)

// configPollInterval is how often the watcher reads the config file. A
// change is applied on the first poll that sees the SAME bytes as the
// previous one, so the worst-case latency is two intervals — irrelevant for
// a config file, and the price of never applying a half-written one.
const configPollInterval = time.Second

// applyResult is what one apply did and what it could not do. Both halves
// are logged: the second is the only feedback anyone gets that an edit has
// not taken effect, since nobody is at a terminal when the watcher runs.
type applyResult struct {
	applied []string
	restart []string
}

// liveApplier turns a parsed config into running state. Everything it can
// reach is a live surface; everything it cannot is named in the log.
type liveApplier struct {
	pool     *pool.Pool
	store    secrets.Store
	notifier *notify.Notifier
	// logLevel is the daemon's slog level, held as a LevelVar so log.level
	// is one more thing that does not need a restart.
	logLevel *slog.LevelVar
	// hostCovered reports whether a host already has a pre-minted MITM leaf
	// — (*proxy.Handler).MITMCovers. nil means "cannot promise coverage",
	// which admin.UpstreamRestartRequired treats as not covered.
	hostCovered  func(host string) bool
	refreshHosts func()
	logger       *slog.Logger

	// mu serialises applies: the admin HTTP goroutine and the watcher
	// goroutine both call apply, and prev is read-modify-written by it.
	mu sync.Mutex
	// prev is the last config successfully applied. The diff against it is
	// what makes "account removed from the file" mean "removed from the
	// pool", and what keeps the log quiet when nothing user-visible moved
	// (the daemon rewrites this file itself on every token refresh).
	prev *config.Config
}

// newLiveApplier baselines against the config the daemon actually started
// from, so the first reload diffs against reality rather than against zero.
func newLiveApplier(startup *config.Config, p *pool.Pool, store secrets.Store,
	n *notify.Notifier, level *slog.LevelVar, logger *slog.Logger) *liveApplier {
	return &liveApplier{
		pool: p, store: store, notifier: n, logLevel: level, logger: logger, prev: startup,
	}
}

// enableLiveMITM wires the proxy handler's MITM bookkeeping, mirroring
// admin.Server.EnableLiveMITM. Separate from the constructor for the same
// reason it is there: tests that exercise the pool side need no MITM at all.
func (a *liveApplier) enableLiveMITM(covers func(host string) bool, refresh func()) {
	a.hostCovered = covers
	a.refreshHosts = refresh
}

// apply makes nc the running configuration, as far as a running process
// can. source names who asked ("config file", "dashboard") and appears in
// the log line.
//
// Application is unconditional and idempotent — pool.Apply and
// SetChannels are re-run whether or not the diff below spotted a change —
// so a gap in the diff logic can only ever cost a log line, never
// correctness. The diff decides what to SAY, not what to do.
func (a *liveApplier) apply(nc *config.Config, source string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	res := &applyResult{}
	prev := a.prev

	// Accounts first: pool.Apply below applies per-account settings by
	// name, so an account added here picks up its disabled/label/priority
	// in the same pass rather than a poll later.
	a.syncAccounts(prev, nc, res)
	a.pool.Apply(poolSettings(nc))
	a.syncNotify(nc, res)
	a.syncLogLevel(prev, nc, res)

	a.warnInlineSecrets(nc, res)

	if prev != nil {
		res.applied = append(res.applied, settingsChanges(prev, nc)...)
		res.restart = append(res.restart, restartOnlyChanges(prev, nc)...)
	}

	a.prev = nc
	a.report(source, res)
}

// report writes the one line anyone gets. INFO when something moved; DEBUG
// otherwise, because the daemon rewrites this file itself every time it
// refreshes a token's expiry and an INFO line per refresh would bury the
// ones that matter.
func (a *liveApplier) report(source string, res *applyResult) {
	if a.logger == nil {
		return
	}
	if len(res.applied) == 0 && len(res.restart) == 0 {
		a.logger.Debug("config reloaded, nothing live-visible changed", "source", source)
		return
	}
	args := []any{"source", source}
	if len(res.applied) > 0 {
		args = append(args, "applied", strings.Join(res.applied, "; "))
	}
	if len(res.restart) > 0 {
		args = append(args, "restart_required", strings.Join(res.restart, "; "))
	}
	a.logger.Info("config reloaded", args...)
}

// syncAccounts adds and removes pool accounts to match the file.
//
// The diff is against the PREVIOUS config, not against the pool: an account
// the pool has that no config ever mentioned is the zero-accounts bootstrap
// fallback (buildPool's "local" keychain login), and a reload must not
// delete it just because it was never in the yaml.
func (a *liveApplier) syncAccounts(prev, nc *config.Config, res *applyResult) {
	if prev == nil {
		return
	}
	next := accountsByName(nc.Accounts)
	for _, old := range prev.Accounts {
		if _, ok := next[old.Name]; ok {
			continue
		}
		// Immediately, before anything can select it: the credential
		// backing it may already have been deleted from the store (issue
		// #83's original report).
		if a.pool.Remove(old.Name) {
			res.applied = append(res.applied, fmt.Sprintf("account %q removed from rotation", old.Name))
		}
	}
	previous := accountsByName(prev.Accounts)
	for _, ac := range nc.Accounts {
		if _, ok := previous[ac.Name]; ok {
			continue
		}
		a.addAccount(ac, res)
	}

	// The zero-accounts bootstrap fallback — buildPool's borrowed claude CLI
	// login, which appears in no config — is invisible to the diff above,
	// deliberately: a reload must not delete a pool account just because the
	// yaml never mentioned it. But once the file does name accounts, a
	// restart would drop it, and until then the pool can still rotate onto a
	// credential spillway is not allowed to refresh (#81). Say so rather
	// than let the difference between "restarted" and "reloaded" be silent.
	//
	// Only on the transition, not on every reload afterwards: buildPool
	// creates the fallback exactly when the config named no accounts, so
	// "the file went from naming none to naming some" is the one moment the
	// difference appears. Repeating it on every subsequent reload would put
	// an INFO line on every token-refresh rewrite of the file.
	if len(prev.Accounts) > 0 || len(nc.Accounts) == 0 {
		return
	}
	for _, existing := range a.pool.Accounts() {
		if existing.Source != pool.SourceKeychain {
			continue
		}
		if _, ok := next[existing.Name]; ok {
			continue
		}
		res.restart = append(res.restart, fmt.Sprintf(
			"account %q is the startup fallback borrowing the claude CLI's own login and is in no config — "+
				"it stays in rotation until a restart", existing.Name))
	}
}

// addAccount puts one newly-configured account into the running pool, or
// explains why it could not.
//
// The upstream-host exception is the one #83's matrix already had and the
// one an unattended reload has to handle differently: a host with no
// pre-minted MITM leaf can only be covered by regenerating the whole chain,
// which strands every already-running proxied CLI (#70). The rule itself is
// admin.UpstreamRestartRequired — the same function the live account-add
// endpoint asks, so the two cannot drift — but the response here is to
// refuse the add entirely and keep running with the old account set. The
// admin endpoint adds it anyway and returns the caveat, which is right when
// a person just ran `spillway login` and is reading the reply; nobody is
// reading anything here, and an account whose CONNECT traffic silently
// blind-tunnels unpooled is precisely the outcome an unattended reload must
// not create.
func (a *liveApplier) addAccount(ac config.AccountConfig, res *applyResult) {
	if ac.Source == "keychain" {
		res.restart = append(res.restart, fmt.Sprintf(
			"account %q NOT added: source: keychain borrows the claude CLI's own login, which is read at startup only",
			ac.Name))
		return
	}
	upstream := ac.Upstream
	if upstream == "" {
		// Same rule as accounts.ResolveYAML: only a provider that owns its
		// own upstream gets defaulted, so a claude account is judged
		// against the global upstream that was covered before the daemon
		// started.
		if spec := provider.For(ac.Type); spec.OwnsUpstream {
			upstream = spec.DefaultUpstream
		}
	}
	if restart, reason := admin.UpstreamRestartRequired(upstream, a.hostCovered); restart {
		res.restart = append(res.restart, fmt.Sprintf(
			"account %q NOT added to the running pool — %s; the pool is still serving the previous account set, "+
				"restart spillway to pick this one up", ac.Name, reason))
		return
	}
	acct, err := accounts.ResolveYAML(ac, a.store)
	if err != nil {
		// The error names the account and the command to fix it, never any
		// token material — see secrets.Store.Get.
		res.restart = append(res.restart, fmt.Sprintf("account %q NOT added: %v", ac.Name, err))
		return
	}
	if !a.pool.Add(acct) {
		return // already present under that name; nothing to say
	}
	if a.refreshHosts != nil {
		a.refreshHosts()
	}
	res.applied = append(res.applied, fmt.Sprintf("account %q added to rotation", ac.Name))
}

// warnInlineSecrets reports token material somebody has hand-written into
// the yaml. §5 is that the config holds metadata only and every credential
// lives in the secret store (the OS keychain, or its 0600 file fallback on
// a keyringless Linux) under "account:<name>" / "notify:<channel>", so a
// reload never READS one of these — accounts.ResolveYAML and
// channelCredentialSource always go to the store — which is exactly why it
// has to be said out loud: the value is doing nothing except sitting on
// disk in plaintext.
//
// It is deliberately not scrubbed here. MigrateInlineSecrets does that at
// startup, and it writes to the keychain; an unattended reload is the wrong
// context to start a keychain write (it can block on an unlocked-keychain
// prompt nobody is there to answer). The name is logged, never the value.
func (a *liveApplier) warnInlineSecrets(nc *config.Config, res *applyResult) {
	for _, ac := range nc.Accounts {
		if ac.AccessToken != "" || ac.RefreshToken != "" {
			res.restart = append(res.restart, fmt.Sprintf(
				"account %q has token material written into the config file — it is NOT being used "+
					"(credentials come from the secret store) and is moved out at the next start", ac.Name))
		}
	}
	for _, ch := range nc.Notify.Channels {
		if ch.URL != "" || ch.Topic != "" || ch.Token != "" || ch.UserKey != "" {
			res.restart = append(res.restart, fmt.Sprintf(
				"notify channel %q has credential material written into the config file — it is NOT being used "+
					"(the destination comes from the secret store) and is moved out at the next start", ch.Name))
		}
	}
}

// syncNotify re-resolves every channel against the config and the secret
// store. Always run, never diffed first: a channel's credential can change
// with no config change at all (`spillway notify set` on an existing
// channel keeps the same name, provider and events and writes only the
// secret), and that POSTs to the settings endpoint, which lands here.
func (a *liveApplier) syncNotify(nc *config.Config, res *applyResult) {
	if a.notifier == nil {
		return
	}
	for _, w := range setNotifyChannels(a.notifier, nc, a.store) {
		if a.logger != nil {
			a.logger.Warn(w)
		}
	}
	if a.prev == nil {
		return
	}
	if changes := notifyChanges(a.prev.Notify.Channels, nc.Notify.Channels); len(changes) > 0 {
		res.applied = append(res.applied, changes...)
	}
}

// syncLogLevel retunes the running logger. Held in a slog.LevelVar for
// exactly this — turning debug on to diagnose something should not require
// bouncing the daemon whose behaviour is being diagnosed.
func (a *liveApplier) syncLogLevel(prev, nc *config.Config, res *applyResult) {
	if a.logLevel == nil || prev == nil {
		return
	}
	want := parseLevel(nc.Log.Level)
	if a.logLevel.Level() == want {
		return
	}
	a.logLevel.Set(want)
	res.applied = append(res.applied, fmt.Sprintf("log.level %s -> %s", prev.Log.Level, nc.Log.Level))
}

// notifyChanges describes what moved in the channel list. Names, providers
// and events only — every one of them already in the file, none of them
// secret (§5).
func notifyChanges(prev, next []config.NotifyChannelConfig) []string {
	prevBy := map[string]config.NotifyChannelConfig{}
	for _, c := range prev {
		prevBy[c.Name] = c
	}
	nextBy := map[string]config.NotifyChannelConfig{}
	for _, c := range next {
		nextBy[c.Name] = c
	}
	var out []string
	for _, c := range prev {
		if _, ok := nextBy[c.Name]; !ok {
			out = append(out, fmt.Sprintf("notify channel %q removed — it will not fire again", c.Name))
		}
	}
	for _, c := range next {
		old, ok := prevBy[c.Name]
		if !ok {
			out = append(out, fmt.Sprintf("notify channel %q (%s) is now live", c.Name, c.Provider))
			continue
		}
		if old.Provider != c.Provider {
			out = append(out, fmt.Sprintf("notify channel %q provider %s -> %s", c.Name, old.Provider, c.Provider))
		}
		if strings.Join(old.Events, ",") != strings.Join(c.Events, ",") {
			out = append(out, fmt.Sprintf("notify channel %q events [%s] -> [%s]",
				c.Name, strings.Join(old.Events, ","), strings.Join(c.Events, ",")))
		}
	}
	return out
}

// settingsChanges names the live pool settings that moved. Purely for the
// log — pool.Apply has already applied all of them unconditionally.
func settingsChanges(prev, next *config.Config) []string {
	var out []string
	if prev.Pool.SwitchThreshold != next.Pool.SwitchThreshold {
		out = append(out, fmt.Sprintf("pool.switchThreshold %v -> %v", prev.Pool.SwitchThreshold, next.Pool.SwitchThreshold))
	}
	if prev.Pool.CrossProvider != next.Pool.CrossProvider {
		out = append(out, fmt.Sprintf("pool.crossProvider %v -> %v", prev.Pool.CrossProvider, next.Pool.CrossProvider))
	}
	if prev.Pool.AllowOverage != next.Pool.AllowOverage {
		// The money switch: always worth its own line.
		out = append(out, fmt.Sprintf("pool.allowOverage %v -> %v", prev.Pool.AllowOverage, next.Pool.AllowOverage))
	}
	if prev.Pool.StickyAcrossFamily != next.Pool.StickyAcrossFamily {
		out = append(out, fmt.Sprintf("pool.stickyAcrossFamily %v -> %v", prev.Pool.StickyAcrossFamily, next.Pool.StickyAcrossFamily))
	}
	if prev.Pool.HideOverageFromClient != next.Pool.HideOverageFromClient {
		out = append(out, fmt.Sprintf("pool.hideOverageFromClient %v -> %v", prev.Pool.HideOverageFromClient, next.Pool.HideOverageFromClient))
	}
	prevBy := accountsByName(prev.Accounts)
	for _, a := range next.Accounts {
		old, ok := prevBy[a.Name]
		if !ok {
			continue // reported by syncAccounts as an add
		}
		if old.Disabled != a.Disabled {
			state := "enabled"
			if a.Disabled {
				state = "disabled"
			}
			out = append(out, fmt.Sprintf("account %q %s", a.Name, state))
		}
		if old.Priority != a.Priority {
			out = append(out, fmt.Sprintf("account %q priority %d -> %d", a.Name, old.Priority, a.Priority))
		}
		if old.Label != a.Label {
			out = append(out, fmt.Sprintf("account %q relabelled", a.Name))
		}
		if !sameBoolPtr(old.AllowOverage, a.AllowOverage) {
			out = append(out, fmt.Sprintf("account %q allowOverage %s -> %s",
				a.Name, boolPtrText(old.AllowOverage), boolPtrText(a.AllowOverage)))
		}
	}
	return out
}

// restartOnlyChanges names every edit a running process cannot honour.
// These are the fields something read exactly once at startup and kept: a
// bound listener, the reverse-proxy handler's own snapshot of the upstream,
// the egress dialer, the background loops' tick intervals. Saying so is the
// entire point — an edit that silently does nothing is what issue #84 is
// about.
func restartOnlyChanges(prev, next *config.Config) []string {
	checks := []struct {
		changed bool
		field   string
		why     string
	}{
		{prev.Proxy.Port != next.Proxy.Port, "proxy.port", "the proxy listener is already bound"},
		{prev.Proxy.Host != next.Proxy.Host, "proxy.host", "the proxy listener is already bound"},
		{prev.Proxy.AllowRemote != next.Proxy.AllowRemote, "proxy.allowRemote", "the proxy listener is already bound"},
		{prev.Admin.Addr != next.Admin.Addr, "admin.addr", "the admin listener is already bound"},
		{prev.Admin.Token != next.Admin.Token, "admin.token", "the admin listener's authentication is fixed at bind"},
		{prev.Upstream != next.Upstream, "upstream", "the proxy handler and the MITM leaf set are built from it at startup"},
		{prev.Egress.Mode != next.Egress.Mode, "egress.mode", "the egress dialer is built at startup"},
		{prev.Egress.Proxy != next.Egress.Proxy, "egress.proxy", "the egress dialer is built at startup"},
		{prev.Pool.ExhaustedMode != next.Pool.ExhaustedMode, "pool.exhaustedMode", "the proxy handler snapshots it at startup"},
		{prev.Pool.HoldMax != next.Pool.HoldMax, "pool.holdMax", "the proxy handler snapshots it at startup"},
		{prev.Pool.MaxBufferBytes != next.Pool.MaxBufferBytes, "pool.maxBufferBytes", "the proxy handler snapshots it at startup"},
		{!sameBoolPtr(prev.Pool.ProbeOnStart, next.Pool.ProbeOnStart), "pool.probeOnStart", "the probe loop is started once"},
		{prev.Pool.ProbeInterval != next.Pool.ProbeInterval, "pool.probeInterval", "the probe loop's ticker is created once"},
		{prev.Pool.CanaryInterval != next.Pool.CanaryInterval, "pool.canaryInterval", "the canary loop's ticker is created once"},
	}
	var out []string
	for _, c := range checks {
		if c.changed {
			out = append(out, fmt.Sprintf("%s needs a restart (%s)", c.field, c.why))
		}
	}
	prevBy := accountsByName(prev.Accounts)
	for _, a := range next.Accounts {
		old, ok := prevBy[a.Name]
		if !ok {
			continue
		}
		switch {
		case old.Upstream != a.Upstream:
			out = append(out, fmt.Sprintf(
				"account %q upstream changed — needs a restart (its MITM leaf and its pool entry are both fixed at startup)", a.Name))
		case old.Type != a.Type:
			out = append(out, fmt.Sprintf("account %q type changed — needs a restart", a.Name))
		case old.Source != a.Source:
			out = append(out, fmt.Sprintf("account %q source changed — needs a restart", a.Name))
		case !sameModelMap(old.ModelMap, a.ModelMap):
			out = append(out, fmt.Sprintf(
				"account %q modelMap changed — needs a restart (it is read without a lock on the request path)", a.Name))
		}
	}
	return out
}

func accountsByName(accts []config.AccountConfig) map[string]config.AccountConfig {
	m := make(map[string]config.AccountConfig, len(accts))
	for _, a := range accts {
		m[a.Name] = a
	}
	return m
}

func sameBoolPtr(a, b *bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func boolPtrText(v *bool) string {
	if v == nil {
		return "pool default"
	}
	if *v {
		return "on"
	}
	return "off"
}

func sameModelMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// configWatcher notices the config file changing underneath a running
// daemon and hands the result to apply.
//
// It polls and hashes rather than subscribing to filesystem events, which
// is a deliberate trade. Editors and spillway's own writer both replace the
// file by atomic rename, so an inode-level watch stops firing after the
// first change and a directory-level one has to filter and re-establish
// per platform; polling re-reads the path every tick and is immune to the
// whole class by construction. It also adds no dependency to a repo with
// four of them, and cannot leak a watch descriptor. The cost is up to two
// seconds of latency on a config file, which nothing cares about.
//
// A change is applied only once the file has stopped moving: a poll that
// sees different bytes records them and waits, and only a poll that sees
// the SAME bytes as the one before applies. That is the debounce (an editor
// emits several writes per save) and it is also what keeps a half-written
// file out of the running config — together with Validate, which is the
// real guarantee.
type configWatcher struct {
	path   string
	apply  func(*config.Config)
	logger *slog.Logger

	// seen is the hash of the bytes the previous poll read; pending marks
	// that those bytes have not been acted on yet.
	seen    [32]byte
	pending bool
	// applied is the fingerprint of the config currently in force —
	// config.Fingerprint, i.e. a hash of the config's MEANING. This is what
	// stops the daemon acting on its own tail: it rewrites this file itself
	// on every token refresh and every dashboard settings write, and a
	// rewrite that parses to the config already loaded is not a change.
	// Comparing meanings rather than bytes also ignores a reformat.
	applied [32]byte
	// lastErr de-duplicates the invalid-config warning, so an editor left
	// holding a broken file does not produce a line every second.
	lastErr string
	missing bool
}

func newConfigWatcher(path string, startup *config.Config, apply func(*config.Config), logger *slog.Logger) *configWatcher {
	return &configWatcher{
		path:    path,
		apply:   apply,
		logger:  logger,
		applied: config.Fingerprint(startup),
	}
}

// poll performs one check. It reports whether watching should continue —
// false once a reload has turned watching off.
func (w *configWatcher) poll() bool {
	data, err := os.ReadFile(w.path)
	if err != nil {
		// Deleted, or unreadable for a moment. Keep the running config:
		// re-deriving one from defaults here would empty the pool.
		if !w.missing {
			w.logger.Warn("config file unreadable — the running configuration is unchanged",
				"path", w.path, "err", err)
			w.missing = true
		}
		w.seen = [32]byte{}
		w.pending = false
		return true
	}
	w.missing = false

	h := sha256.Sum256(data)
	if h != w.seen {
		// Changed since the last poll: it may still be being written. Wait
		// for one quiet interval before reading it as a finished edit.
		w.seen = h
		w.pending = true
		return true
	}
	if !w.pending {
		return true // stable, and already dealt with
	}
	w.pending = false

	// Validate gates everything. A file that does not parse, or that parses
	// into something Validate rejects, leaves the running config untouched.
	cfg, err := config.ParseValidate(data)
	if err != nil {
		if msg := err.Error(); msg != w.lastErr {
			w.lastErr = msg
			w.logger.Warn("config file changed but is not valid — the running configuration is unchanged",
				"path", w.path, "err", err)
		}
		return true
	}
	w.lastErr = ""

	if fp := config.Fingerprint(cfg); fp != w.applied {
		w.applied = fp
		w.apply(cfg)
	}
	return cfg.WatchEnabled()
}

// run polls until ctx is cancelled, or until a reload turns watching off.
// Returns rather than lingering, so nothing is left running after shutdown.
func (w *configWatcher) run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !w.poll() {
				w.logger.Info("no longer watching the config file (watchConfig: false) — "+
					"external edits now take effect at the next restart", "path", w.path)
				return
			}
		}
	}
}
