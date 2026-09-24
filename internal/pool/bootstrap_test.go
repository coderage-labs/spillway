package pool

// Issue #131: DisplaceBootstrap is the single implementation of "the first
// genuinely configured account displaces the zero-accounts startup
// fallback". Both live routes call it and nothing else, so these are the
// rule's own cases.

import (
	"testing"
	"time"
)

func bootstrapFor(name string) *Account {
	a := NewAccount(name, SourceKeychain, "borrowed", "borrowed-refresh", 0, "")
	a.Type = "claude-oauth"
	a.SetBootstrap(true)
	return a
}

func configured(name string) *Account {
	a := NewAccount(name, SourceYAML, "tok", "refresh", 0, "")
	a.Type = "claude-oauth"
	return a
}

func names(p *Pool) []string {
	var out []string
	for _, a := range p.Accounts() {
		out = append(out, a.Name)
	}
	return out
}

func TestDisplaceBootstrap(t *testing.T) {
	tests := []struct {
		name      string
		accounts  []*Account
		wantGone  string
		wantNames []string
	}{
		{
			name:      "fallback beside a configured account is displaced",
			accounts:  []*Account{bootstrapFor("local"), configured("work")},
			wantGone:  "local",
			wantNames: []string{"work"},
		},
		{
			// Order must not matter: the rule is about what the accounts
			// ARE, not where they sit in the slice.
			name:      "fallback added after the configured account is displaced",
			accounts:  []*Account{configured("work"), bootstrapFor("local")},
			wantGone:  "local",
			wantNames: []string{"work"},
		},
		{
			// The pool must never be emptied. A borrowed credential still
			// serves; nothing does not.
			name:      "fallback alone is kept",
			accounts:  []*Account{bootstrapFor("local")},
			wantGone:  "",
			wantNames: []string{"local"},
		},
		{
			// Only the flag decides. A deliberate `source: keychain` account
			// shares the fallback's Source and may share its name.
			name:      "an unflagged keychain account is never displaced",
			accounts:  []*Account{NewAccount("local", SourceKeychain, "t", "r", 0, ""), configured("work")},
			wantGone:  "",
			wantNames: []string{"local", "work"},
		},
		{
			name:      "no fallback at all is a no-op",
			accounts:  []*Account{configured("one"), configured("two")},
			wantGone:  "",
			wantNames: []string{"one", "two"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := New(tc.accounts, time.Now())
			if got := p.DisplaceBootstrap(); got != tc.wantGone {
				t.Errorf("DisplaceBootstrap() = %q, want %q", got, tc.wantGone)
			}
			got := names(p)
			if len(got) != len(tc.wantNames) {
				t.Fatalf("pool = %v, want %v", got, tc.wantNames)
			}
			for i := range got {
				if got[i] != tc.wantNames[i] {
					t.Fatalf("pool = %v, want %v", got, tc.wantNames)
				}
			}
			// Idempotent: apply runs unconditionally on every reload.
			if again := p.DisplaceBootstrap(); again != "" {
				t.Errorf("a second call removed %q as well", again)
			}
		})
	}
}

// TestDisplacedFallbackCannotBeSelected: gone from the running pool, not
// merely absent from a listing. This repo's signature failure is a change
// that looks right in one layer and never reaches the other.
func TestDisplacedFallbackCannotBeSelected(t *testing.T) {
	p := New([]*Account{bootstrapFor("local"), configured("work")}, time.Now())
	// Sticky session already pointing at the fallback, as a live daemon's
	// would be after serving from it.
	if sel := p.SelectFor("session-1", nil); sel == nil {
		t.Fatal("nothing selectable before the displacement")
	} else {
		p.Done(sel)
	}

	if gone := p.DisplaceBootstrap(); gone != "local" {
		t.Fatalf("DisplaceBootstrap() = %q, want local", gone)
	}
	if err := p.Pin("local", false); err == nil {
		t.Fatal("the displaced fallback is still resolvable by name")
	}
	for i := 0; i < 50; i++ {
		sel := p.SelectFor("session-1", nil)
		if sel == nil {
			t.Fatal("nothing selectable after the displacement")
		}
		if sel.Name == "local" {
			t.Fatal("a sticky session is still being routed to the displaced fallback")
		}
		p.Done(sel)
	}
}
