package admin

// Issue #131, the admin half: POST /api/accounts/add must drop the
// zero-accounts bootstrap fallback — the borrowed claude CLI login that
// lives in no config — the moment a genuinely configured account arrives,
// so a live add and a restart reach the same pool.
//
// The cross-route agreement proof lives in cmd/spillway (it needs buildPool
// and the config watcher too). These are the cases only reachable from this
// side of the wire.

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/events"
	"github.com/coderage-labs/spillway/internal/pool"
)

func bootstrapAccount(name string) *pool.Account {
	a := pool.NewAccount(name, pool.SourceKeychain, "borrowed-tok", "borrowed-refresh", 0, "")
	a.Type = "claude-oauth"
	a.SetBootstrap(true)
	return a
}

func addAndDecode(t *testing.T, s *Server, body string) accountAddResponse {
	t.Helper()
	rec := postAccountAdd(t, s, body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var got accountAddResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

// TestAccountAddDisplacesTheBootstrapFallback: the borrowed login leaves the
// running pool, and the response says so.
func TestAccountAddDisplacesTheBootstrapFallback(t *testing.T) {
	p := pool.New([]*pool.Account{bootstrapAccount("local")}, time.Now())
	s := New(p, nil, events.New(), "")

	got := addAndDecode(t, s, `{"name":"work","type":"claude-oauth","accessToken":"configured","refreshToken":"r"}`)
	if !got.Added {
		t.Fatal("added=false for a brand new name")
	}
	if got.Displaced != "local" {
		t.Errorf("displaced = %q, want local — the caller has to be told their borrowed login left rotation", got.Displaced)
	}

	accts := p.Accounts()
	if len(accts) != 1 || accts[0].Name != "work" {
		t.Fatalf("pool = %v, want only the configured account", accountNames(p))
	}
	if err := p.Pin("local", false); err == nil {
		t.Error("the displaced fallback is still resolvable — it can still be selected for a request")
	}
	for i := 0; i < 20; i++ {
		sel := p.SelectFor("s", nil)
		if sel == nil {
			t.Fatal("nothing selectable after the displacement")
		}
		if sel.Token() == "borrowed-tok" {
			t.Fatal("a request routed to the borrowed claude CLI credential")
		}
		p.Done(sel)
	}
}

// TestReauthUnderTheFallbackNameStopsItBeingTheFallback: logging in as
// "local" gives spillway its own grant for that account and writes it to the
// config, so it is a genuinely configured pool member from then on. If the
// flag survived, the NEXT ordinary add would displace a real account.
func TestReauthUnderTheFallbackNameStopsItBeingTheFallback(t *testing.T) {
	local := bootstrapAccount("local")
	p := pool.New([]*pool.Account{local}, time.Now())
	s := New(p, nil, events.New(), "")

	first := addAndDecode(t, s, `{"name":"local","type":"claude-oauth","accessToken":"own-grant","refreshToken":"own-refresh"}`)
	if first.Added {
		t.Error("added=true for a name the pool already holds — this is a hot-swap, not a new entry")
	}
	if n := len(p.Accounts()); n != 1 {
		t.Fatalf("pool has %d accounts after a re-auth, want 1 — it must still be able to serve", n)
	}
	if local.IsBootstrap() {
		t.Fatal("still flagged as the borrowed startup fallback after spillway obtained its own grant for it")
	}
	if tok := local.Token(); tok != "own-grant" {
		t.Fatalf("token = %q, want the credential just posted", tok)
	}

	got := addAndDecode(t, s, `{"name":"work","type":"claude-oauth","accessToken":"configured","refreshToken":"r"}`)
	if got.Displaced != "" {
		t.Fatalf("displaced %q — a re-authenticated, configured account was treated as the throwaway fallback", got.Displaced)
	}
	if n := len(p.Accounts()); n != 2 {
		t.Fatalf("pool = %v, want both accounts", accountNames(p))
	}
}

// TestAccountAddNeverDisplacesAnUnflaggedKeychainAccount: a deliberate
// `source: keychain` account shares the fallback's Source, and may share its
// name. Only the flag distinguishes them, and only the flag may decide.
func TestAccountAddNeverDisplacesAnUnflaggedKeychainAccount(t *testing.T) {
	configured := pool.NewAccount("local", pool.SourceKeychain, "tok", "r", 0, "")
	configured.Type = "claude-oauth"
	p := pool.New([]*pool.Account{configured}, time.Now())
	s := New(p, nil, events.New(), "")

	got := addAndDecode(t, s, `{"name":"work","type":"claude-oauth","accessToken":"configured","refreshToken":"r"}`)
	if got.Displaced != "" {
		t.Fatalf("displaced %q — a configured source: keychain account is not the bootstrap fallback", got.Displaced)
	}
	if n := len(p.Accounts()); n != 2 {
		t.Fatalf("pool = %v, want both accounts", accountNames(p))
	}
}

func accountNames(p *pool.Pool) []string {
	var out []string
	for _, a := range p.Accounts() {
		out = append(out, a.Name)
	}
	return out
}
