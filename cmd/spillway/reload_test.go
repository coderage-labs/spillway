package main

// Tests for issue #84's config reload. Everything here drives the REAL
// config parser, the REAL pool, the REAL notifier and a real on-disk file —
// no mock of the thing under test. A mock that encodes the wrong
// precondition is how a green test hides a live bug, and this repo has
// shipped two features that passed their tests and did nothing in
// production.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/admin"
	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/events"
	"github.com/coderage-labs/spillway/internal/notify"
	"github.com/coderage-labs/spillway/internal/pool"
	"github.com/coderage-labs/spillway/internal/secrets"
)

// syncBuffer is a log sink safe to read while a delivery goroutine writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// harness wires a real applier over a real pool, secret store, notifier and
// config file.
type harness struct {
	t        *testing.T
	path     string
	pool     *pool.Pool
	store    *secrets.Fake
	notifier *notify.Notifier
	applier  *liveApplier
	level    *slog.LevelVar
	logs     *syncBuffer
}

func newHarness(t *testing.T, yaml string, accts ...*pool.Account) *harness {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "spillway.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFrom(path)
	if err != nil {
		t.Fatalf("initial config: %v", err)
	}
	p := pool.New(accts, time.Now())
	store := secrets.NewFake()
	n := notify.New()
	logs := &syncBuffer{}
	level := new(slog.LevelVar)
	level.Set(slog.LevelInfo)
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: level}))
	n.SetLogger(logger)
	return &harness{
		t: t, path: path, pool: p, store: store, notifier: n, logs: logs, level: level,
		applier: newLiveApplier(cfg, p, store, n, level, logger),
	}
}

// write replaces the config file in place, the way `cat > file` does.
func (h *harness) write(yaml string) {
	h.t.Helper()
	if err := os.WriteFile(h.path, []byte(yaml), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

// renameOver replaces the config file by atomic rename — what every real
// editor and spillway's own writeFile actually do.
func (h *harness) renameOver(yaml string) {
	h.t.Helper()
	tmp := h.path + ".new"
	if err := os.WriteFile(tmp, []byte(yaml), 0o600); err != nil {
		h.t.Fatal(err)
	}
	if err := os.Rename(tmp, h.path); err != nil {
		h.t.Fatal(err)
	}
}

// reload parses the file on disk and applies it, exactly as the watcher's
// apply callback does.
func (h *harness) reload() {
	h.t.Helper()
	data, err := os.ReadFile(h.path)
	if err != nil {
		h.t.Fatal(err)
	}
	cfg, err := config.ParseValidate(data)
	if err != nil {
		h.t.Fatalf("config no longer valid: %v", err)
	}
	h.applier.apply(cfg, "config file")
}

// watcher returns a watcher over this harness's file, baselined on what is
// currently loaded, applying through the same applier.
func (h *harness) watcher() *configWatcher {
	h.t.Helper()
	logger := slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: h.level}))
	return newConfigWatcher(h.path, h.applier.prev, func(nc *config.Config) {
		h.applier.apply(nc, "config file")
	}, logger)
}

func acct(name string) *pool.Account {
	a := pool.NewAccount(name, pool.SourceYAML, "tok", "refresh", 0, "")
	a.Type = "claude-oauth"
	return a
}

func poolNames(p *pool.Pool) []string {
	var out []string
	for _, a := range p.Accounts() {
		out = append(out, a.Name)
	}
	return out
}

const twoAccounts = `accounts:
  - name: one
    type: claude-oauth
  - name: two
    type: claude-oauth
`

// ---------------------------------------------------------------------------
// The watcher: rename, validation, self-writes, debounce, shutdown.
// ---------------------------------------------------------------------------

// TestWatcherSeesRepeatedAtomicRenames is the trap the issue calls out. An
// editor — and spillway's own config.writeFile — replaces the file rather
// than modifying it, so a watch bound to the inode fires once and then goes
// deaf. This proves a SECOND rename over the same path is still seen.
func TestWatcherSeesRepeatedAtomicRenames(t *testing.T) {
	h := newHarness(t, twoAccounts, acct("one"), acct("two"))
	w := h.watcher()

	var seen []float64
	w.apply = func(nc *config.Config) { seen = append(seen, nc.Pool.SwitchThreshold) }

	h.renameOver(twoAccounts + "pool:\n  switchThreshold: 0.5\n")
	pollUntilStable(w)
	if len(seen) != 1 || seen[0] != 0.5 {
		t.Fatalf("after first rename, applied = %v; want [0.5]", seen)
	}

	// The one that matters: the file the watcher first saw no longer
	// exists, and a brand new inode is at the path.
	h.renameOver(twoAccounts + "pool:\n  switchThreshold: 0.25\n")
	pollUntilStable(w)
	if len(seen) != 2 || seen[1] != 0.25 {
		t.Fatalf("after second rename, applied = %v; want [0.5 0.25] — the watch went deaf after the first replacement", seen)
	}
}

// pollUntilStable runs the two polls a settled change needs: one to notice
// the new bytes, one to confirm they stopped moving.
func pollUntilStable(w *configWatcher) {
	w.poll()
	w.poll()
}

// TestWatcherLeavesRunningConfigIntactOnInvalidYAML: a half-written or
// broken file must never reach the pool. Asserted against the pool's actual
// state, not just against "the callback didn't run".
func TestWatcherLeavesRunningConfigIntactOnInvalidYAML(t *testing.T) {
	h := newHarness(t, twoAccounts+"pool:\n  switchThreshold: 0.5\n", acct("one"), acct("two"))
	h.reload() // baseline: 0.5 is live
	if got := h.pool.Threshold(); got != 0.5 {
		t.Fatalf("setup: threshold = %v, want 0.5", got)
	}
	w := h.watcher()

	// Truncated mid-write: this is what a poll can catch an editor doing.
	h.write("accounts:\n  - name: one\n    type: claude-oa")
	pollUntilStable(w)

	if got := h.pool.Threshold(); got != 0.5 {
		t.Fatalf("threshold = %v after an invalid config; the running config must be untouched", got)
	}
	if names := poolNames(h.pool); len(names) != 2 {
		t.Fatalf("pool = %v after an invalid config; want both accounts still present", names)
	}
	if !strings.Contains(h.logs.String(), "not valid") {
		t.Fatalf("nothing logged about the invalid config; log was:\n%s", h.logs.String())
	}

	// And it recovers: a later valid edit is still applied.
	h.write(twoAccounts + "pool:\n  switchThreshold: 0.75\n")
	pollUntilStable(w)
	if got := h.pool.Threshold(); got != 0.75 {
		t.Fatalf("threshold = %v; a valid config after a broken one must apply", got)
	}
}

// TestWatcherIgnoresRewriteOfTheSameConfig: spillway rewrites this file
// itself (a refreshed token's expiry, a dashboard settings write). A
// rewrite whose MEANING matches what is loaded must not be re-applied —
// including one that differs only in formatting, which is why the
// fingerprint hashes the parsed config rather than the bytes.
func TestWatcherIgnoresRewriteOfTheSameConfig(t *testing.T) {
	h := newHarness(t, twoAccounts, acct("one"), acct("two"))
	w := h.watcher()

	applies := 0
	w.apply = func(*config.Config) { applies++ }

	// Byte-identical rewrite.
	h.renameOver(twoAccounts)
	pollUntilStable(w)
	if applies != 0 {
		t.Fatalf("applies = %d after an identical rewrite; want 0", applies)
	}

	// Same meaning, different bytes: reordered keys, a comment, different
	// indentation, an explicitly spelled-out default.
	h.renameOver("# managed by something else\n" +
		"log:\n    level: info\n" +
		"accounts:\n" +
		"    - type: claude-oauth\n      name: one\n" +
		"    - type: claude-oauth\n      name: two\n")
	pollUntilStable(w)
	if applies != 0 {
		t.Fatalf("applies = %d after a formatting-only rewrite; want 0 — the fingerprint is hashing bytes, not meaning", applies)
	}

	// A real change still gets through, so the above is not just inertia.
	h.renameOver(twoAccounts + "pool:\n  switchThreshold: 0.3\n")
	pollUntilStable(w)
	if applies != 1 {
		t.Fatalf("applies = %d after a real change; want 1", applies)
	}
}

// TestWatcherDebouncesABurstOfWrites: an editor emits several writes per
// save. Only the settled result may be applied, and only once.
func TestWatcherDebouncesABurstOfWrites(t *testing.T) {
	h := newHarness(t, twoAccounts, acct("one"), acct("two"))
	w := h.watcher()

	var seen []float64
	w.apply = func(nc *config.Config) { seen = append(seen, nc.Pool.SwitchThreshold) }

	// Three writes, a poll between each — the burst.
	h.write(twoAccounts + "pool:\n  switchThreshold: 0.1\n")
	w.poll()
	h.write(twoAccounts + "pool:\n  switchThreshold: 0.2\n")
	w.poll()
	h.write(twoAccounts + "pool:\n  switchThreshold: 0.3\n")
	w.poll()
	if len(seen) != 0 {
		t.Fatalf("applied %v mid-burst; nothing may be applied while the file is still moving", seen)
	}

	w.poll() // the file has stopped moving
	if len(seen) != 1 || seen[0] != 0.3 {
		t.Fatalf("applied %v; want exactly one apply of the settled value 0.3", seen)
	}
}

// TestWatcherKeepsRunningConfigWhenFileDisappears: a deleted config must
// not be silently recreated from defaults and applied — that would empty
// the pool. (config.LoadFrom would do exactly that, which is why the
// watcher uses ParseValidate.)
func TestWatcherKeepsRunningConfigWhenFileDisappears(t *testing.T) {
	h := newHarness(t, twoAccounts, acct("one"), acct("two"))
	w := h.watcher()

	if err := os.Remove(h.path); err != nil {
		t.Fatal(err)
	}
	pollUntilStable(w)
	pollUntilStable(w)

	if names := poolNames(h.pool); len(names) != 2 {
		t.Fatalf("pool = %v after the config file vanished; want both accounts untouched", names)
	}
	if _, err := os.Stat(h.path); err == nil {
		t.Fatal("the watcher recreated the config file; it must never write one")
	}
}

// TestWatcherRunStopsOnContextCancel guards against leaving a goroutine
// behind at shutdown.
func TestWatcherRunStopsOnContextCancel(t *testing.T) {
	h := newHarness(t, twoAccounts, acct("one"))
	w := h.watcher()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.run(ctx, time.Millisecond); close(done) }()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher goroutine did not exit after its context was cancelled")
	}
}

// TestWatcherStopsWhenWatchingIsTurnedOff: the opt-out takes effect on the
// running daemon rather than at the next restart.
func TestWatcherStopsWhenWatchingIsTurnedOff(t *testing.T) {
	h := newHarness(t, twoAccounts, acct("one"))
	w := h.watcher()
	h.write(twoAccounts + "watchConfig: false\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { w.run(ctx, time.Millisecond); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run() kept polling after watchConfig: false")
	}
	if !strings.Contains(h.logs.String(), "no longer watching") {
		t.Fatalf("stopping was not reported:\n%s", h.logs.String())
	}
}

// ---------------------------------------------------------------------------
// The applier: accounts, the upstream exception, notify, restart-only.
// ---------------------------------------------------------------------------

// TestReloadRemovesAccountFromRunningPool: an account deleted from the file
// by anything at all stops being selectable, without a restart. Asserted
// against selection, not against a list.
func TestReloadRemovesAccountFromRunningPool(t *testing.T) {
	h := newHarness(t, twoAccounts, acct("one"), acct("two"))

	h.write("accounts:\n  - name: two\n    type: claude-oauth\n")
	h.reload()

	for i := 0; i < 20; i++ {
		got := h.pool.SelectFor(fmt.Sprintf("s%d", i), nil)
		if got == nil {
			t.Fatal("nothing selectable after the reload")
		}
		if got.Name == "one" {
			t.Fatal("a removed account is still being selected for requests")
		}
		h.pool.Done(got)
	}
	if !strings.Contains(h.logs.String(), `account \"one\" removed from rotation`) {
		t.Fatalf("removal not reported in the log:\n%s", h.logs.String())
	}
}

// TestReloadAddsAccountToRunningPool: an account appended to the file, with
// its credential already in the secret store, becomes selectable with no
// restart. The credential is read from the store — never from the file.
func TestReloadAddsAccountToRunningPool(t *testing.T) {
	h := newHarness(t, "accounts:\n  - name: one\n    type: claude-oauth\n", acct("one"))
	if err := h.store.Set("two", secrets.Secrets{AccessToken: "at", RefreshToken: "rt"}); err != nil {
		t.Fatal(err)
	}

	h.write(twoAccounts)
	h.reload()

	var found *pool.Account
	for _, a := range h.pool.Accounts() {
		if a.Name == "two" {
			found = a
		}
	}
	if found == nil {
		t.Fatalf("account not added to the running pool; pool = %v", poolNames(h.pool))
	}
	if got := found.Token(); got != "at" {
		t.Fatalf("added account's token = %q; want the one from the secret store", got)
	}
	// Selectable, not merely present.
	h.pool.Pin("two", true)
	if got := h.pool.SelectFor("s", nil); got == nil || got.Name != "two" {
		t.Fatalf("SelectFor = %v; a newly configured account must be selectable immediately", got)
	}
}

// TestReloadWithoutCredentialDoesNotAddAccount: an account hand-written
// into the yaml with nothing in the secret store cannot serve. It must be
// refused with an explanation, never added as a credential-less entry that
// fails on a real request.
func TestReloadWithoutCredentialDoesNotAddAccount(t *testing.T) {
	h := newHarness(t, "accounts:\n  - name: one\n    type: claude-oauth\n", acct("one"))

	h.write(twoAccounts)
	h.reload()

	if names := poolNames(h.pool); len(names) != 1 {
		t.Fatalf("pool = %v; an account with no credential must not enter rotation", names)
	}
	if !strings.Contains(h.logs.String(), "NOT added") {
		t.Fatalf("the refusal was not logged:\n%s", h.logs.String())
	}
}

// TestReloadRefusesAccountWithUncoveredUpstreamHost is the exception the
// issue singles out. A new upstream host needs a MITM leaf, which after #70
// means regenerating the whole chain and stranding every running session.
// Unattended, that must not happen silently: the account is refused, the
// old set keeps serving, and the log says what was refused and that a
// restart is needed.
func TestReloadRefusesAccountWithUncoveredUpstreamHost(t *testing.T) {
	h := newHarness(t, "accounts:\n  - name: one\n    type: claude-oauth\n", acct("one"))
	if err := h.store.Set("custom", secrets.Secrets{AccessToken: "at"}); err != nil {
		t.Fatal(err)
	}
	// Only api.anthropic.com has a pre-minted leaf, the way it would after
	// a normal start.
	h.applier.enableLiveMITM(func(host string) bool { return host == "api.anthropic.com" }, func() {})

	h.write("accounts:\n" +
		"  - name: one\n    type: claude-oauth\n" +
		"  - name: custom\n    type: claude-oauth\n    upstream: https://llm.internal.example\n")
	h.reload()

	if names := poolNames(h.pool); len(names) != 1 || names[0] != "one" {
		t.Fatalf("pool = %v; an account on an uncovered upstream host must NOT be added — "+
			"its CONNECT traffic would blind-tunnel unpooled", names)
	}
	log := h.logs.String()
	if !strings.Contains(log, "NOT added") || !strings.Contains(log, "llm.internal.example") {
		t.Fatalf("the refusal must name the account and the host:\n%s", log)
	}
	if !strings.Contains(log, "restart") {
		t.Fatalf("the refusal must say a restart is needed:\n%s", log)
	}
}

// TestReloadAddsAccountOnAlreadyCoveredUpstream is the other half: a host
// that already has a pre-minted leaf is live-safe and must not be caught by
// the refusal above. Without this, "refuse new upstreams" would quietly
// mean "never add an account".
func TestReloadAddsAccountOnAlreadyCoveredUpstream(t *testing.T) {
	h := newHarness(t, "accounts:\n  - name: one\n    type: claude-oauth\n", acct("one"))
	if err := h.store.Set("two", secrets.Secrets{AccessToken: "at"}); err != nil {
		t.Fatal(err)
	}
	h.applier.enableLiveMITM(func(host string) bool { return host == "api.anthropic.com" }, func() {})

	h.write("accounts:\n" +
		"  - name: one\n    type: claude-oauth\n" +
		"  - name: two\n    type: claude-oauth\n    upstream: https://api.anthropic.com\n")
	h.reload()

	if names := poolNames(h.pool); len(names) != 2 {
		t.Fatalf("pool = %v; an account on an already-covered host is live-safe and must be added", names)
	}
}

// TestReloadStartsANewNotifyChannelWithoutARestart is the concrete case
// that motivated issue #84: `spillway notify set phone` wrote the config
// and the credential, and the channel did nothing until the daemon was
// restarted. A real webhook server proves the channel actually fires.
func TestReloadStartsANewNotifyChannelWithoutARestart(t *testing.T) {
	got := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 4096)
		n, _ := r.Body.Read(body)
		got <- string(body[:n])
	}))
	defer srv.Close()

	h := newHarness(t, twoAccounts, acct("one"))
	blob, _ := json.Marshal(notify.Destination{URL: srv.URL})
	if err := h.store.SetRaw(notify.ChannelKey("phone"), blob); err != nil {
		t.Fatal(err)
	}

	h.write(twoAccounts + "notify:\n  channels:\n    - name: phone\n      provider: webhook\n      events: [held]\n")
	h.reload()

	h.notifier.Notify(notify.EventHeld, "k1", "spillway", "a request is held")
	select {
	case <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("a notify channel added to the config never fired — it still needs a restart")
	}
	if !strings.Contains(h.logs.String(), `notify channel \"phone\" (webhook) is now live`) {
		t.Fatalf("the new channel was not reported:\n%s", h.logs.String())
	}
}

// TestReloadStopsARemovedNotifyChannel is the other direction: a channel
// deleted from the config must stop firing immediately. Two channels are
// configured and only one removed, so the surviving channel's delivery is
// the barrier that proves the fan-out ran at all.
func TestReloadStopsARemovedNotifyChannel(t *testing.T) {
	goneHits := make(chan struct{}, 4)
	gone := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		goneHits <- struct{}{}
	}))
	defer gone.Close()
	keptHits := make(chan struct{}, 4)
	kept := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		keptHits <- struct{}{}
	}))
	defer kept.Close()

	both := "notify:\n  channels:\n" +
		"    - name: gone\n      provider: webhook\n      events: [held]\n" +
		"    - name: kept\n      provider: webhook\n      events: [held]\n"
	h := newHarness(t, twoAccounts+both, acct("one"))
	for name, url := range map[string]string{"gone": gone.URL, "kept": kept.URL} {
		blob, _ := json.Marshal(notify.Destination{URL: url})
		if err := h.store.SetRaw(notify.ChannelKey(name), blob); err != nil {
			t.Fatal(err)
		}
	}
	h.reload()

	// Both live to begin with.
	h.notifier.Notify(notify.EventHeld, "k1", "t", "b")
	waitHit(t, goneHits, "gone channel before removal")
	waitHit(t, keptHits, "kept channel before removal")

	h.write(twoAccounts + "notify:\n  channels:\n" +
		"    - name: kept\n      provider: webhook\n      events: [held]\n")
	h.reload()

	h.notifier.Notify(notify.EventHeld, "k2", "t", "b")
	waitHit(t, keptHits, "kept channel after removal")
	select {
	case <-goneHits:
		t.Fatal("a notify channel removed from the config is still firing")
	default:
	}
}

func waitHit(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("no delivery to the %s", what)
	}
}

// TestReloadNeverLogsOrWritesASecret is §5 as a test. A reload reads
// credentials out of the secret store; not one of them may reach the log or
// the config file.
func TestReloadNeverLogsOrWritesASecret(t *testing.T) {
	const (
		webhookSecret = "https://hooks.example.invalid/T0P-S3CR3T-PATH"
		bearerSecret  = "tk_super_secret_bearer"
		topicSecret   = "9f2c1a4e7b0d5f83aa11bb22cc33dd44"
		accessSecret  = "sk-ant-oat01-DO-NOT-LOG-ME"
	)
	h := newHarness(t, twoAccounts, acct("one"))
	blob, _ := json.Marshal(notify.Destination{URL: webhookSecret, Token: bearerSecret})
	if err := h.store.SetRaw(notify.ChannelKey("phone"), blob); err != nil {
		t.Fatal(err)
	}
	ntfyBlob, _ := json.Marshal(notify.Destination{Topic: topicSecret})
	if err := h.store.SetRaw(notify.ChannelKey("tablet"), ntfyBlob); err != nil {
		t.Fatal(err)
	}
	if err := h.store.Set("three", secrets.Secrets{AccessToken: accessSecret, RefreshToken: accessSecret}); err != nil {
		t.Fatal(err)
	}

	h.write(twoAccounts +
		"  - name: three\n    type: claude-oauth\n" +
		"notify:\n  channels:\n" +
		"    - name: phone\n      provider: webhook\n      events: [held]\n" +
		"    - name: tablet\n      provider: ntfy\n      events: [exhausted]\n" +
		"    - name: nocred\n      provider: pushover\n      events: [held]\n")
	h.reload()
	// A second reload with an account whose credential is missing, so the
	// error-reporting path is exercised too.
	h.reload()

	log := h.logs.String()
	for _, secret := range []string{webhookSecret, bearerSecret, topicSecret, accessSecret} {
		if strings.Contains(log, secret) {
			t.Fatalf("a reload logged secret material (%q):\n%s", secret, log)
		}
	}
	onDisk, err := os.ReadFile(h.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{webhookSecret, bearerSecret, topicSecret, accessSecret} {
		if strings.Contains(string(onDisk), secret) {
			t.Fatalf("a reload wrote secret material into the config file (%q)", secret)
		}
	}
	// The channel names are not secret and must still be reported, or the
	// test above would pass on a reload that did nothing at all.
	if !strings.Contains(log, "phone") || !strings.Contains(log, "tablet") {
		t.Fatalf("expected the channel names in the log:\n%s", log)
	}
}

// TestReloadReportsRestartOnlyChanges: an edit a running process cannot
// honour must say so. Silence is the failure mode issue #84 exists to fix.
func TestReloadReportsRestartOnlyChanges(t *testing.T) {
	h := newHarness(t, twoAccounts, acct("one"))

	h.write(twoAccounts + "proxy:\n  port: 9999\nupstream: https://elsewhere.example\n" +
		"pool:\n  exhaustedMode: fail\n  holdMax: 1h\n")
	h.reload()

	log := h.logs.String()
	for _, want := range []string{"restart_required", "proxy.port", "upstream", "pool.exhaustedMode", "pool.holdMax"} {
		if !strings.Contains(log, want) {
			t.Fatalf("restart-only change %q not reported:\n%s", want, log)
		}
	}
}

// TestReloadAppliesPoolSettingsAndLogLevel covers the ordinary live path,
// including log.level — which used to be fixed for the life of the process.
func TestReloadAppliesPoolSettingsAndLogLevel(t *testing.T) {
	h := newHarness(t, twoAccounts, acct("one"))

	h.write(twoAccounts + "log:\n  level: debug\npool:\n  switchThreshold: 0.42\n  allowOverage: true\n")
	h.reload()

	if got := h.pool.Threshold(); got != 0.42 {
		t.Fatalf("switchThreshold = %v; want 0.42 live", got)
	}
	if !h.pool.AllowOverage() {
		t.Fatal("pool.allowOverage did not reach the running pool")
	}
	if h.level.Level() != slog.LevelDebug {
		t.Fatalf("log level = %v; want debug applied live", h.level.Level())
	}
	if !strings.Contains(h.logs.String(), "log.level info -> debug") {
		t.Fatalf("log level change not reported:\n%s", h.logs.String())
	}
}

// TestReloadOfAnUnchangedConfigSaysNothing: the daemon rewrites this file
// itself on every token refresh. An INFO line per refresh would bury the
// ones that matter.
func TestReloadOfAnUnchangedConfigSaysNothing(t *testing.T) {
	h := newHarness(t, twoAccounts, acct("one"), acct("two"))
	h.reload()
	if got := h.logs.String(); strings.Contains(got, "config reloaded") {
		t.Fatalf("an unchanged reload logged at INFO:\n%s", got)
	}
}

// TestNotifySetReachesTheRunningDaemon is the reported bug end to end, over
// the wire: a channel written to the config while the daemon is up must
// fire without a restart. It drives a REAL admin.Server backed by the REAL
// applier and calls the same helper `spillway notify set` calls, so a
// regression anywhere on that path — the CLI helper, the settings endpoint,
// the applier, the notifier — fails here.
func TestNotifySetReachesTheRunningDaemon(t *testing.T) {
	hits := make(chan struct{}, 4)
	webhook := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hits <- struct{}{}
	}))
	defer webhook.Close()

	p := pool.New([]*pool.Account{acct("one")}, time.Now())
	srv := admin.New(p, nil, events.New(), "")
	front := httptest.NewServer(srv)
	defer front.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "spillway.yaml")
	base := "admin:\n  addr: " + strings.TrimPrefix(front.URL, "http://") + "\n" +
		"accounts:\n  - name: one\n    type: claude-oauth\n"
	if err := os.WriteFile(cfgPath, []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPILLWAY_CONFIG", cfgPath)

	cfg, err := config.LoadFrom(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	store := secrets.NewFake()
	blob, _ := json.Marshal(notify.Destination{URL: webhook.URL})
	if err := store.SetRaw(notify.ChannelKey("phone"), blob); err != nil {
		t.Fatal(err)
	}
	n := notify.New()
	logs := &syncBuffer{}
	level := new(slog.LevelVar)
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: level}))
	applier := newLiveApplier(cfg, p, store, n, level, logger)
	srv.EnableSettings(cfgPath, func(nc *config.Config) { applier.apply(nc, "dashboard") })

	// What `spillway notify set phone` does: credential to the store (above),
	// metadata to the config, then tell the daemon.
	if err := config.UpsertNotifyChannel(cfgPath, config.NotifyChannelConfig{
		Name: "phone", Provider: "webhook", Events: []string{notify.EventHeld},
	}); err != nil {
		t.Fatal(err)
	}
	if msg := liveApplyConfigEdit(); !strings.Contains(msg, "no restart needed") {
		t.Fatalf("liveApplyConfigEdit said %q; want the applied-immediately message", msg)
	}

	n.Notify(notify.EventHeld, "k1", "spillway", "a request is held")
	select {
	case <-hits:
	case <-time.After(10 * time.Second):
		t.Fatal("`notify set` did not reach the running daemon — the channel still needs a restart")
	}
}

// TestReloadReportsTheBootstrapFallbackAccount: running with no accounts
// configured, spillway borrows the claude CLI's own login as a single
// "local" account. It is in no config, so the reload diff cannot see it —
// and must not delete it on that basis. Once the file names real accounts a
// restart WOULD drop it, and until then the pool can still rotate onto a
// credential spillway is not allowed to refresh (#81), so the difference
// has to be said rather than left silent.
func TestReloadReportsTheBootstrapFallbackAccount(t *testing.T) {
	local := pool.NewAccount("local", pool.SourceKeychain, "tok", "", 0, "")
	local.Type = "claude-oauth"
	h := newHarness(t, "log:\n  level: info\n", local)
	if err := h.store.Set("work", secrets.Secrets{AccessToken: "at"}); err != nil {
		t.Fatal(err)
	}

	h.write("accounts:\n  - name: work\n    type: claude-oauth\n")
	h.reload()

	if names := poolNames(h.pool); len(names) != 2 {
		t.Fatalf("pool = %v; the bootstrap fallback must not be deleted by a reload, and the new account must be added", names)
	}
	log := h.logs.String()
	if !strings.Contains(log, "startup fallback") || !strings.Contains(log, "restart_required") {
		t.Fatalf("the still-borrowed fallback account was not reported as needing a restart:\n%s", log)
	}

	// Said once, at the transition — not on every reload afterwards. The
	// daemon rewrites this file on every token refresh, and a line per
	// refresh would bury the ones that matter.
	mark := len(log)
	h.write("accounts:\n  - name: work\n    type: claude-oauth\n    label: day job\n")
	h.reload()
	if after := h.logs.String()[mark:]; strings.Contains(after, "startup fallback") {
		t.Fatalf("the fallback notice repeated on a later reload:\n%s", after)
	}
}

// TestReloadReportsInlineSecretsWithoutLoggingThem: §5 says the config holds
// metadata only. A token hand-written into the yaml is never used — every
// credential comes from the secret store — so a reload has to say it is
// sitting there in plaintext, and must not quote it while doing so.
func TestReloadReportsInlineSecretsWithoutLoggingThem(t *testing.T) {
	const inlineToken = "sk-ant-oat01-INLINE-SHOULD-NEVER-BE-LOGGED"
	const inlineTopic = "abc123def456topic"
	h := newHarness(t, twoAccounts, acct("one"), acct("two"))

	h.write("accounts:\n" +
		"  - name: one\n    type: claude-oauth\n    accessToken: " + inlineToken + "\n" +
		"  - name: two\n    type: claude-oauth\n" +
		"notify:\n  channels:\n    - name: phone\n      provider: ntfy\n      topic: " + inlineTopic + "\n")
	h.reload()

	log := h.logs.String()
	if !strings.Contains(log, "token material written into the config file") {
		t.Fatalf("an inline account token was not reported:\n%s", log)
	}
	if !strings.Contains(log, "credential material written into the config file") {
		t.Fatalf("an inline channel credential was not reported:\n%s", log)
	}
	if strings.Contains(log, inlineToken) || strings.Contains(log, inlineTopic) {
		t.Fatalf("the report quoted the secret it was warning about:\n%s", log)
	}
	// And the inline value is not used: the account keeps the credential the
	// pool already resolved from the store, not the one in the file.
	for _, a := range h.pool.Accounts() {
		if a.Name == "one" && a.Token() == inlineToken {
			t.Fatal("a reload used token material out of the config file")
		}
	}
}
