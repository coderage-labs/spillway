package main

// Issue #131: the zero-accounts bootstrap fallback — the claude CLI's own
// login, borrowed, named "local" and written to no config file — used to
// survive a live account-add. A restart dropped it (buildPool only
// synthesises it when the config names nothing), a live add did not, so the
// same config produced two different pools depending on how it got there.
//
// The defect is the DIVERGENCE, so the test that matters drives all three
// routes — restart, POST /api/accounts/add, config reload — from the same
// starting pool through the same config change, and compares them against
// each other. A test exercising one route would miss the entire point.
//
// Everything here uses the real buildPool (through its injectable seam), the
// real admin handler, the real liveApplier and a real pool. Nothing opens
// the user's keychain: accounts.Source is an interface and the fake below is
// the only credential source in play.

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/accounts"
	"github.com/coderage-labs/spillway/internal/admin"
	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/events"
	"github.com/coderage-labs/spillway/internal/notify"
	"github.com/coderage-labs/spillway/internal/pool"
	"github.com/coderage-labs/spillway/internal/secrets"
)

// borrowedLogin stands in for the claude CLI's credential store — the thing
// buildPool reads when the config names no accounts. Its token is distinct
// from every configured account's, so "which credential is in the pool" is
// answerable, not inferred.
func borrowedLogin(t *testing.T) accounts.Source {
	t.Helper()
	blob := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"borrowed-cli-token","refreshToken":"borrowed-refresh",`+
		`"expiresAt":%d,"subscriptionType":"max","scopes":["user:inference"]}}`,
		time.Now().Add(24*time.Hour).UnixMilli())
	return accounts.SourceFunc(func() ([]byte, error) { return []byte(blob), nil })
}

func quietLogger() *slog.Logger {
	lv := new(slog.LevelVar)
	lv.Set(slog.LevelError)
	return slog.New(slog.NewTextHandler(&syncBuffer{}, &slog.HandlerOptions{Level: lv}))
}

// bootstrapPool is what a daemon that started with an empty config is
// running: a pool built by the real buildPool from the borrowed CLI login.
func bootstrapPool(t *testing.T, store secrets.Store, cfgPath string) (*pool.Pool, *config.Config) {
	t.Helper()
	empty := config.Defaults()
	p, err := buildPoolFrom(&empty, cfgPath, borrowedLogin(t), store, quietLogger(), time.Now(), notify.New())
	if err != nil {
		t.Fatalf("buildPool with an empty config: %v", err)
	}
	return p, &empty
}

func sortedPoolNames(p *pool.Pool) []string {
	names := poolNames(p)
	sort.Strings(names)
	return names
}

// bootstrapNames reports the names of pool members still flagged as the
// borrowed startup fallback.
func bootstrapNames(p *pool.Pool) []string {
	var out []string
	for _, a := range p.Accounts() {
		if a.IsBootstrap() {
			out = append(out, a.Name)
		}
	}
	sort.Strings(out)
	return out
}

const oneConfiguredAccount = "accounts:\n  - name: work\n    type: claude-oauth\n"

// TestBootstrapFallbackAgreesAcrossRestartLiveAddAndReload is issue #131's
// actual defect: restart and live-add reaching different pool states from
// the same config. Each route starts from a daemon booted with an empty
// config (so the pool holds the borrowed "local" fallback and nothing else)
// and is given the same one-account config. All three must land on the same
// pool.
func TestBootstrapFallbackAgreesAcrossRestartLiveAddAndReload(t *testing.T) {
	cfg, err := config.ParseValidate([]byte(oneConfiguredAccount))
	if err != nil {
		t.Fatal(err)
	}

	newStore := func(t *testing.T) *secrets.Fake {
		t.Helper()
		st := secrets.NewFake()
		if err := st.Set("work", secrets.Secrets{AccessToken: "configured-token", RefreshToken: "configured-refresh"}); err != nil {
			t.Fatal(err)
		}
		return st
	}

	routes := []struct {
		name string
		// build returns the pool that route leaves running.
		build func(t *testing.T) *pool.Pool
	}{
		{
			// The baseline every other route has to match: what the daemon
			// would be running had it been restarted with this config.
			name: "restart",
			build: func(t *testing.T) *pool.Pool {
				store := newStore(t)
				cfgPath := filepath.Join(t.TempDir(), "spillway.yaml")
				p, err := buildPoolFrom(cfg, cfgPath, borrowedLogin(t), store, quietLogger(), time.Now(), notify.New())
				if err != nil {
					t.Fatalf("buildPool: %v", err)
				}
				return p
			},
		},
		{
			name: "live add via POST /api/accounts/add",
			build: func(t *testing.T) *pool.Pool {
				store := newStore(t)
				cfgPath := filepath.Join(t.TempDir(), "spillway.yaml")
				p, _ := bootstrapPool(t, store, cfgPath)
				s := admin.New(p, nil, events.New(), "")
				body := `{"name":"work","type":"claude-oauth","accessToken":"configured-token",` +
					`"refreshToken":"configured-refresh","priority":0}`
				rec := httptest.NewRecorder()
				s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
					"http://127.0.0.1:7657/api/accounts/add", strings.NewReader(body)))
				if rec.Code != http.StatusOK {
					t.Fatalf("account add returned %d: %s", rec.Code, rec.Body.String())
				}
				var resp struct {
					Added     bool   `json:"added"`
					Displaced string `json:"displaced"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
					t.Fatal(err)
				}
				if !resp.Added {
					t.Fatal("account add reported added=false for a brand new name")
				}
				if resp.Displaced != "local" {
					t.Fatalf("displaced = %q; the endpoint must SAY the borrowed login left rotation", resp.Displaced)
				}
				return p
			},
		},
		{
			name: "config reload by the watcher",
			build: func(t *testing.T) *pool.Pool {
				store := newStore(t)
				cfgPath := filepath.Join(t.TempDir(), "spillway.yaml")
				p, startup := bootstrapPool(t, store, cfgPath)
				level := new(slog.LevelVar)
				applier := newLiveApplier(startup, p, store, notify.New(), level, quietLogger())
				applier.apply(cfg, "config file")
				return p
			},
		},
	}

	var baseline []string
	for _, r := range routes {
		t.Run(r.name, func(t *testing.T) {
			p := r.build(t)
			names := sortedPoolNames(p)

			if got := bootstrapNames(p); len(got) != 0 {
				t.Errorf("borrowed startup fallback %v still in the pool after a genuinely configured account arrived", got)
			}
			if len(names) != 1 || names[0] != "work" {
				t.Errorf("pool = %v, want [work]", names)
			}
			// Gone from the RUNNING pool, not merely from a listing.
			if err := p.Pin("local", false); err == nil {
				t.Error("the fallback is still resolvable by the pool — it can still be selected for a request")
			}
			for i := 0; i < 20; i++ {
				sel := p.SelectFor(fmt.Sprintf("s%d", i), nil)
				if sel == nil {
					t.Fatal("nothing selectable — the pool cannot serve")
				}
				if tok := sel.Token(); tok == "borrowed-cli-token" {
					t.Fatalf("request %d routed to the borrowed claude CLI credential", i)
				}
				p.Done(sel)
			}

			if baseline == nil {
				baseline = names
				return
			}
			if strings.Join(baseline, ",") != strings.Join(names, ",") {
				t.Fatalf("this route leaves pool %v; the restart route leaves %v — the two must agree", names, baseline)
			}
		})
	}
}

// TestEmptyConfigStillGetsAWorkingPool: the genuine bootstrap case. A user
// with no accounts configured must still get a pool, from the borrowed CLI
// login, and nothing about the displacement rule may take it away while it
// is the only thing there.
func TestEmptyConfigStillGetsAWorkingPool(t *testing.T) {
	store := secrets.NewFake()
	p, _ := bootstrapPool(t, store, filepath.Join(t.TempDir(), "spillway.yaml"))

	accts := p.Accounts()
	if len(accts) != 1 {
		t.Fatalf("pool = %v; an empty config must yield exactly the borrowed login", poolNames(p))
	}
	if accts[0].Name != "local" {
		t.Errorf("fallback name = %q, want local", accts[0].Name)
	}
	if !accts[0].IsBootstrap() {
		t.Error("the fallback is not flagged as the bootstrap account — nothing can displace what it cannot identify")
	}
	if tok := accts[0].Token(); tok != "borrowed-cli-token" {
		t.Errorf("fallback token = %q, want the borrowed CLI credential", tok)
	}
	sel := p.SelectFor("s", nil)
	if sel == nil || sel.Name != "local" {
		t.Fatalf("SelectFor = %v; the bootstrap pool must actually serve", sel)
	}
	p.Done(sel)

	// The rule must not empty a pool whose only member is the fallback.
	if gone := p.DisplaceBootstrap(); gone != "" {
		t.Fatalf("DisplaceBootstrap removed %q with nothing to replace it", gone)
	}
	if len(p.Accounts()) != 1 {
		t.Fatalf("pool = %v after DisplaceBootstrap; want the fallback kept", poolNames(p))
	}
}

// TestConfiguredAccountsSuppressTheBootstrapFallback: the startup half of
// the same rule. With accounts in the config, buildPool must never reach for
// the CLI's login at all.
func TestConfiguredAccountsSuppressTheBootstrapFallback(t *testing.T) {
	store := secrets.NewFake()
	if err := store.Set("work", secrets.Secrets{AccessToken: "configured-token", RefreshToken: "rt"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.ParseValidate([]byte(oneConfiguredAccount))
	if err != nil {
		t.Fatal(err)
	}
	p, err := buildPoolFrom(cfg, filepath.Join(t.TempDir(), "spillway.yaml"),
		accounts.SourceFunc(func() ([]byte, error) {
			t.Error("buildPool read the claude CLI login although the config names an account")
			return nil, fmt.Errorf("must not be called")
		}), store, quietLogger(), time.Now(), notify.New())
	if err != nil {
		t.Fatalf("buildPool: %v", err)
	}
	if names := sortedPoolNames(p); len(names) != 1 || names[0] != "work" {
		t.Fatalf("pool = %v, want [work]", names)
	}
	if got := bootstrapNames(p); len(got) != 0 {
		t.Fatalf("buildPool flagged %v as a bootstrap fallback although the config names accounts", got)
	}
}

// TestBootstrapFallbackDoesNotShiftTheFirstRealAccountsPriority re-checks
// issue #169's stated assumption after #131's change: NextPriority
// deliberately does not count the bootstrap fallback, and cannot, because it
// has no config entry. Displacing the fallback does not give it one — this
// change removes it from the POOL and writes nothing to the file — so the
// first genuinely configured account still lands at priority 0, the top
// tier, exactly as an empty pool's first account should.
func TestBootstrapFallbackDoesNotShiftTheFirstRealAccountsPriority(t *testing.T) {
	store := secrets.NewFake()
	if err := store.Set("work", secrets.Secrets{AccessToken: "configured-token", RefreshToken: "rt"}); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(t.TempDir(), "spillway.yaml")
	p, _ := bootstrapPool(t, store, cfgPath)
	if got := bootstrapNames(p); len(got) != 1 {
		t.Fatalf("expected the borrowed fallback in the pool, got %v", got)
	}

	// What `spillway login` writes while that fallback is running.
	prio, err := config.UpsertAccountWithPriority(cfgPath,
		config.AccountConfig{Name: "work", Type: "claude-oauth"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if prio != 0 {
		t.Fatalf("first configured account got priority %d, want 0 — the fallback must not be counted", prio)
	}

	cfg, err := config.LoadFrom(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	level := new(slog.LevelVar)
	startup := config.Defaults()
	newLiveApplier(&startup, p, store, notify.New(), level, quietLogger()).apply(cfg, "config file")

	accts := p.Accounts()
	if len(accts) != 1 || accts[0].Name != "work" {
		t.Fatalf("pool = %v, want [work]", poolNames(p))
	}
	if accts[0].Priority() != 0 {
		t.Fatalf("pool priority = %d, want 0 — the pool must rank it exactly as the config records it",
			accts[0].Priority())
	}
}

// TestLoginSaysTheBorrowedFallbackLeftRotation drives the whole
// `spillway login` route — the CLI helper, a real HTTP admin server, and a
// real pool built by buildPool from the borrowed CLI login. The person
// running the command has to be told the credential they were pooling
// against is no longer in rotation; silence is how it gets discovered later.
func TestLoginSaysTheBorrowedFallbackLeftRotation(t *testing.T) {
	store := secrets.NewFake()
	p, _ := bootstrapPool(t, store, filepath.Join(t.TempDir(), "spillway.yaml"))

	srv := admin.New(p, nil, events.New(), "")
	front := httptest.NewServer(srv)
	defer front.Close()
	liveTestConfig(t, strings.TrimPrefix(front.URL, "http://"), "local")

	msg := liveAddAccount(accountAddPayload{
		Name: "work", Type: "claude-oauth", AccessToken: "configured-token",
		RefreshToken: "configured-refresh", ExpiresAt: 12345,
	})
	if !strings.Contains(msg, "left rotation") || !strings.Contains(msg, `"local"`) {
		t.Fatalf("login said %q; it must name the borrowed fallback that left the pool", msg)
	}
	if names := sortedPoolNames(p); len(names) != 1 || names[0] != "work" {
		t.Fatalf("pool = %v, want [work]", names)
	}
}
