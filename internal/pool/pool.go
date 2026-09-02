// Package pool tracks account health and selects accounts for requests.
//
// Rotation policy is sticky-until-exhausted (design doc §6.2): a session
// stays on one account for prompt-cache affinity and rotates only when that
// account reports quota exhaustion. Selection runs under a mutex with
// per-account in-flight counters (§6.3); on contention the account with the
// fewest in-flight requests wins.
package pool

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coderage-labs/spillway/internal/provider"
)

// State is an account's eligibility state.
type State int

const (
	// StateOK serves requests.
	StateOK State = iota
	// StateExhausted is out of quota until ExhaustedUntil.
	StateExhausted
	// StateDisabled never serves requests (e.g. dead refresh token).
	StateDisabled
)

// Credential sources.
const (
	// SourceYAML is an account whose tokens live in spillway.yaml; refreshes
	// are written back to that file.
	SourceYAML = "yaml"
	// SourceKeychain is the local claude CLI login. The CLI owns that
	// keychain item and refreshes it itself — we only ever reload from it,
	// never write.
	SourceKeychain = "keychain"
)

// Account is one pooled subscription account.
type Account struct {
	Name     string
	Upstream string
	// parked is an operator disable from config. Kept separate from
	// StateDisabled, which means the credential died: un-parking must never
	// revive an account whose token is dead.
	parked bool
	// LastModel is the model this account most recently served, after any
	// modelMap rewrite — the answer to "what am I actually talking to".
	lastModel string
	// priority orders selection: lower is preferred. Equal priorities fall
	// back to least-loaded, so this is a preference, not a queue. Guarded by
	// mu: the dashboard settings handler writes it from the admin HTTP
	// goroutine while the selector reads it (via Priority/better) from
	// whatever goroutine is picking an account for a request — an unguarded
	// field here raced (issue #13).
	priority int
	// overage is the provider's last word on serving past quota at cost.
	// Guarded by mu.
	overage provider.Overage
	// allowOverage permits spending on this account, overriding the pool
	// default. nil means "follow the pool" — the tri-state is deliberate, so
	// that an account can opt out of a pool-wide yes. Guarded by mu for the
	// same reason as priority — this is the money switch (issue #13): a torn
	// read here decides whether the request bills.
	allowOverage *bool
	// label is an optional human name for display; the dashboard falls back
	// to deriving one from Name when it is empty. Guarded by mu (issue #13).
	label string
	// Source records where credentials live (SourceYAML / SourceKeychain).
	Source string
	// Type is the provider kind ("claude-oauth" / "kimi-oauth") — error
	// classification and refresh endpoints are provider-specific.
	Type string
	// ModelMap rewrites the request body's model id — both the top-level
	// model and any advisor model nested in tools[] (issue #29) — when
	// serving through this account (§4 allowed mutations #3 and #4,
	// cross-provider only). nil for same-provider accounts.
	ModelMap map[string]string
	// AccountUUID is the UUID Claude Code embeds in metadata.user_id for
	// this account; empty disables the rewrite (pass through rather than
	// guess). Immutable after wiring.
	AccountUUID string

	mu             sync.Mutex
	accessToken    string
	refreshToken   string
	expiresAt      int64 // epoch milliseconds
	state          State
	exhaustedUntil time.Time
	inFlight       int
	quota          map[string]string // last seen anthropic-ratelimit-* headers
	quotaAt        time.Time
	windows        []QuotaWindow // provider-agnostic quota state (§6.5)
	// windowRejected records a confirmed upstream refusal per window name,
	// keyed by the window's spillway name ("7d-fable"), until it resets
	// (issue #54's correction to #24). Distinct from windows[]'s
	// Used/Limit, which is a proactive, PREFER-not-refuse signal
	// (OverThresholdFor): this is upstream having already said no for
	// certain, so it must EXCLUDE selection for the family that window
	// governs — including when the account is the only one in the pool —
	// rather than merely deprioritise it.
	windowRejected map[string]time.Time
	// probeBackoff is the current spacing enforced between exhausted-account
	// re-probes (issue #90): zero until the first re-probe is rejected
	// again, then baseInterval, then doubling on every further rejected
	// re-probe (capped by maxProbeBackoff) — so a genuinely spent account is
	// probed less and less often instead of every tick forever. Cleared to
	// zero by ClearExhausted, never by MarkReprobeRejected itself: a
	// rejected re-probe must not reset its own backoff to the base interval,
	// only a confirmed recovery does.
	probeBackoff time.Duration
	// nextProbeAt is the earliest time a re-probe should run again after a
	// rejected re-probe (issue #90); zero means no extra restriction beyond
	// needsProbe's ordinary staleness check.
	nextProbeAt time.Time
}

// QuotaWindow is one provider quota bucket in a provider-agnostic shape:
// anthropic-ratelimit headers and kimi /usages both fill this (§6.5).
type QuotaWindow struct {
	// Name is the window identifier in one vocabulary across providers:
	// "5h", "7d", "7d-fable". Providers that name their windows differently
	// are normalised at the edge, in their own spec, so nothing downstream
	// has to know that Kimi calls a week something else.
	Name      string    `json:"name"`
	Limit     float64   `json:"limit"`   // 0 when the provider doesn't report one
	Used      float64   `json:"used"`    // consumed units (for header windows: utilization, Limit=1)
	ResetAt   time.Time `json:"resetAt"` // zero when unknown
	Source    string    `json:"source"`  // "headers" (anthropic responses) or "poll" (kimi /usages)
	FetchedAt time.Time `json:"fetchedAt"`
	// Expired is true when ResetAt has passed and nothing has re-measured
	// the window since (issue #135) — a reading that no longer says anything
	// about current state. Computed by QuotaWindows() on the copy it
	// returns, never stored: a value a caller writes here is overwritten on
	// the next read. Served over the wire rather than derived per client from
	// resetAt, on the same reasoning as admin/state.go's Threshold: the
	// selector's clock and the dashboard's must agree, and only one of them
	// is here.
	Expired bool `json:"expired,omitempty"`
}

// currentAt reports whether w still says something about the present
// (issue #135): a reading is evidence only until the moment the provider said
// the window would refill. After that spillway holds a number and no way to
// know whether it is still true — for the account-wide windows the next
// request re-measures them, but a spent 7d-fable only ever arrives on a fable
// response, and being spent is exactly what stops fable being routed there,
// so without this the reading was held for the life of the daemon.
//
// Three deliberate edges:
//
//   - A zero ResetAt stays current: a spent window whose reset is unknown
//     keeps deprioritising the account. That is the idle probe's rule
//     (accounts.wouldBill: never spend uninvited) and the opposite result to
//     SeedQuota, which discards such a sample at startup. Both fail toward
//     the side that costs nothing; the cost just sits on opposite sides —
//     seeding a stale "spent" suppresses a probe that should happen, while
//     routing on an unknown reset risks a refusal, or a bill.
//   - A reading fetched at or after its own reset is the newest truth, not a
//     stale one: Anthropic can report a reset that has already passed
//     (anthropicReset anticipates exactly that), and treating such a fresh
//     header as expired would let a fable request land, bill on extra usage,
//     and repeat every turn.
//   - now == ResetAt is expired, matching SeedQuota's !now.Before.
//
// Evaluated lazily at read time, the way WindowRejectedFor already applies
// its deadline. Not done by dropping the row in setWindowsSourced: the row
// still evidences when the window was last measured (the dashboard's age,
// needsProbe's FetchedAt scan), and absence is not deletion (#100). Not
// signalled to parked holds either: an over-threshold account is always
// admitted by tier 2 or 3 of SelectExcept, so expiry can never turn a nil
// selection into a non-nil one — there is nothing to wake.
func (w QuotaWindow) currentAt(now time.Time) bool {
	return w.ResetAt.IsZero() || now.Before(w.ResetAt) || !w.FetchedAt.Before(w.ResetAt)
}

// NewAccount builds an account.
func NewAccount(name, source, accessToken, refreshToken string, expiresAt int64, upstream string) *Account {
	return &Account{
		Name:         name,
		Source:       source,
		Upstream:     upstream,
		accessToken:  accessToken,
		refreshToken: refreshToken,
		expiresAt:    expiresAt,
	}
}

// Token returns the current access token (refresh-safe).
func (a *Account) Token() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.accessToken
}

// Credentials returns all credential fields under one lock.
func (a *Account) Credentials() (access, refresh string, expiresAt int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.accessToken, a.refreshToken, a.expiresAt
}

// SetCredentials replaces the credential fields after a refresh/reload (§6.4:
// persisted by the caller before waiters are released).
func (a *Account) SetCredentials(access, refresh string, expiresAt int64) {
	a.mu.Lock()
	a.accessToken = access
	a.refreshToken = refresh
	a.expiresAt = expiresAt
	// Fresh credentials revive an account disabled for auth failure.
	if a.state == StateDisabled {
		a.state = StateOK
	}
	a.mu.Unlock()
}

// CanRefresh reports whether the account has a recovery path: a refresh
// token (yaml) or a keychain item to reload.
func (a *Account) CanRefresh() bool {
	if a.Source == SourceKeychain {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.refreshToken != ""
}

// IsStaticKey reports whether the account holds a static credential: an
// access token with no refresh token and no expiry (e.g. a console API key).
// Static keys are never refresh-attempted and never disabled for lacking a
// refresh token — they only die when the upstream rejects them (401).
func (a *Account) IsStaticKey() bool {
	if a.Source == SourceKeychain {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.refreshToken == "" && a.expiresAt <= 0
}

// Disable permanently removes the account from rotation (e.g. dead refresh
// token).
func (a *Account) Disable() {
	a.mu.Lock()
	a.state = StateDisabled
	a.mu.Unlock()
}

// TokenManager recovers account credentials. Implemented by
// accounts.Manager; pool only depends on the interface.
type TokenManager interface {
	// EnsureFresh proactively refreshes when expiry is near.
	EnsureFresh(ctx context.Context, a *Account) error
	// Recover forces one refresh/reload after an upstream 401.
	Recover(ctx context.Context, a *Account) error
}

// ErrNoTokenManager is returned by Recover/EnsureFresh when no manager is
// wired (tests, single-account fallback).
var ErrNoTokenManager = errors.New("pool: no token manager configured")

// Pool is a set of accounts with sticky session affinity.
type Pool struct {
	mu       sync.Mutex
	accounts []*Account
	sticky   map[string]string // session key -> account name
	// pinned overrides selection entirely while set (#11).
	pinned string
	// sessionProvider pins a session to the provider it started on. The
	// client configured its capabilities from that first model, so switching
	// families mid-session can change the context ceiling underneath it
	// (§6.18).
	sessionProvider map[string]string
	// crossProvider allows rotation between providers mid-session. Off by
	// default: same-provider rotation is transparent, cross-provider is not.
	// Guarded by mu — see allowOverage below for why (issue #13).
	crossProvider bool
	tm            TokenManager
	// allowOverage permits the last-resort tier: serving from an account
	// whose subscription quota is gone but which may bill for extra usage.
	// Off by default — a proxy that starts spending money because a limit
	// was reached is not something to opt out of after the fact.
	//
	// Guarded by mu. The dashboard settings handler used to assign this (and
	// switchThreshold/crossProvider) as a plain exported field from the admin
	// HTTP goroutine while SelectExcept read it under mu from whatever
	// goroutine was selecting an account — a data race on the field deciding
	// whether a request bills (issue #13). Apply is now the only writer.
	allowOverage bool
	// switchThreshold is the used-fraction (0-1) at or above which an
	// account is skipped while another eligible account exists (§6.5
	// predictive rotation). 0 → default 0.98. Guarded by mu (issue #13).
	switchThreshold float64
	// stickyAcrossFamily keeps a session pinned to its sticky account even
	// when that account's governing window for the request's family is
	// spent, rather than moving to a colder account with headroom (issue
	// #24 decision 2). Off by default: the pool moves, the same way every
	// other predictive-rotation decision in this package already prefers a
	// healthier account over the one on file. On trades a possible refusal
	// for keeping the account's prompt cache warm. Guarded by mu, same as
	// switchThreshold above.
	stickyAcrossFamily bool
	// hideOverageFromClient strips the credit markers — the signals Claude
	// Code's usage-credit gate latches on — from pooled Claude responses
	// before they reach the client (issue #103). Off by default: on, it
	// removes the client's own paid-usage consent dialog, leaving
	// allowOverage above as the only spend authority — which is the point
	// behind a pool, and exactly what the flag's documentation says to the
	// user's face. Guarded by mu, same as switchThreshold above.
	hideOverageFromClient bool
	// holds are the requests currently parked waiting for a reset, guarded
	// by mu. See hold.go.
	holds map[*hold]struct{}
	// capGen is the current generation of the capacity-changed broadcast
	// (issue #105), guarded by mu. See capacity.go.
	capGen *CapacityWake
}

// New builds a Pool. Accounts with expired tokens and no recovery path start
// disabled; refreshable accounts stay eligible and are refreshed at Select
// time (§6.4).
func New(accounts []*Account, now time.Time) *Pool {
	for _, a := range accounts {
		_, _, exp := a.Credentials()
		if exp > 0 && exp <= now.UnixMilli() && !a.CanRefresh() {
			a.state = StateDisabled
		}
	}
	return &Pool{accounts: accounts, sticky: map[string]string{},
		sessionProvider: map[string]string{}, switchThreshold: 0.98,
		capGen: newCapacityWake()}
}

// Threshold is the used-fraction at or above which an account is rotated
// away from while a better one exists. Exported so that what the dashboard
// calls "spent" is decided by the same number the selector uses, rather than
// by a constant copied into the UI.
//
// Locked (unlike threshold, its unexported counterpart called from within
// SelectExcept which already holds mu): callers outside the package have no
// other synchronization with Apply's write.
func (p *Pool) Threshold() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.threshold()
}

// threshold assumes mu is already held.
func (p *Pool) threshold() float64 {
	if p.switchThreshold > 0 {
		return p.switchThreshold
	}
	return 0.98
}

// SetTokenManager wires credential recovery (refresh-on-select, 401
// recovery).
func (p *Pool) SetTokenManager(tm TokenManager) { p.tm = tm }

// EnsureFresh proactively refreshes an account nearing expiry. Called by the
// proxy right after Select — never under the pool mutex.
func (p *Pool) EnsureFresh(ctx context.Context, a *Account) error {
	if p.tm == nil {
		return nil
	}
	wasDisabled := a.State() == StateDisabled
	err := p.tm.EnsureFresh(ctx, a)
	if wasDisabled && a.State() != StateDisabled {
		// Issue #105: a refresh that revives a disabled credential is a
		// transition into potentially-usable capacity, the same as a live
		// re-auth through the admin endpoint.
		p.SignalCapacity()
	}
	return err
}

// Recover forces one credential recovery after an upstream 401.
func (p *Pool) Recover(ctx context.Context, a *Account) error {
	if p.tm == nil {
		return ErrNoTokenManager
	}
	wasDisabled := a.State() == StateDisabled
	err := p.tm.Recover(ctx, a)
	if wasDisabled && a.State() != StateDisabled {
		p.SignalCapacity() // issue #105, same reasoning as EnsureFresh above
	}
	return err
}

// Accounts returns a snapshot of the pool's accounts (shared *Account
// pointers; read state via the Account methods). A copy, not the live slice:
// issue #83 made p.accounts mutable at runtime (Remove, so far — a
// corresponding Add is expected in a follow-up), so handing out the backing
// array itself would race a concurrent removal the instant a caller
// iterated it unlocked, which every caller outside this package does.
func (p *Pool) Accounts() []*Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*Account, len(p.accounts))
	copy(out, p.accounts)
	return out
}

// EarliestReset reports the soonest time an exhausted account becomes
// eligible again; ok=false when no account is merely exhausted (e.g. all
// disabled — waiting won't help).
func (p *Pool) EarliestReset() (time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var earliest time.Time
	ok := false
	for _, a := range p.accounts {
		if a.State() != StateExhausted {
			continue
		}
		if u := a.ExhaustedUntil(); !ok || u.Before(earliest) {
			earliest = u
			ok = true
		}
	}
	return earliest, ok
}

// Select picks the account for a session: the sticky account if still
// eligible and under the switch threshold, else the eligible, under-threshold
// account with the fewest in-flight requests. Over-threshold is a PREFERENCE,
// not a ban: when only over-threshold accounts remain, the least-loaded one
// serves anyway. Returns nil when every account is exhausted or disabled.
// The caller must call Done when the request finishes.
// Select picks the account for a session. Deprecated in favour of SelectFor,
// which can also honour request-shape capability; kept for callers that have
// no body to check.
func (p *Pool) Select(session string) *Account {
	return p.SelectFor(session, nil)
}

// SelectFor picks the account for a session, skipping any that cannot serve
// this request body (design doc §6.19) and, unless CrossProvider is set,
// keeping the session on the provider it started with (§6.18: the client
// configured its capabilities from the first model it saw).
func (p *Pool) SelectFor(session string, body []byte) *Account {
	return p.SelectExcept(session, body, nil)
}

// SelectExcept is SelectFor with a set of account names this request has
// already tried and failed on.
//
// The exclusion is not optional for the overage tier. Every other tier is
// self-limiting — a quota 429 marks the account exhausted, which removes it
// from consideration — but the overage tier deliberately admits exhausted
// accounts, so without a skip set a refused overage request re-selects the
// same account immediately and spins.
func (p *Pool) SelectExcept(session string, body []byte, skip map[string]bool) *Account {
	p.mu.Lock()
	defer p.mu.Unlock()

	// model governs which quota window(s) a request's headroom is judged
	// against (issue #24) — "" (no body, or no top-level model field) reads
	// as an unrecognised model to every provider's GoverningWindows, which
	// resolves it to the general windows rather than guessing narrower.
	model := modelOf(body)

	usable := func(a *Account) bool {
		if skip[a.Name] {
			return false
		}
		if !p.crossProvider {
			if want, ok := p.sessionProvider[session]; ok && ProviderOf(a.Type) != want {
				return false
			}
		}
		// A confirmed per-window rejection excludes outright (issue #54's
		// correction to #24) — unlike OverThresholdFor below, which only
		// deprioritises. Checked here, ahead of every tier AND the pin and
		// sticky paths: a window upstream has already refused for certain
		// must not be handed this request again just because it is the
		// pinned/sticky/only account, or because nothing billable remains.
		if a.WindowRejectedFor(model) {
			return false
		}
		return CanServe(a, body) == nil
	}

	// Before sticky: a pin is an instruction, and honouring the previous
	// choice ahead of it would mean the switch appeared to do nothing until
	// the session happened to rotate on its own.
	if best := p.pinnedChoice(usable); best != nil {
		best.addInFlight(1)
		p.sticky[session] = best.Name
		if p.sessionProvider == nil {
			p.sessionProvider = map[string]string{}
		}
		p.sessionProvider[session] = ProviderOf(best.Type)
		return best
	}

	if name, ok := p.sticky[session]; ok {
		if a := p.find(name); a != nil && a.eligible() && usable(a) {
			// stickyAcrossFamily short-circuits the threshold check
			// entirely: decision 2 (#24) is "stay pinned no matter what",
			// not "stay pinned unless every window is spent" — the whole
			// point is trading a possible refusal for the warm cache.
			if p.stickyAcrossFamily || !a.OverThresholdFor(model, p.threshold()) {
				a.addInFlight(1)
				return a
			}
		}
		delete(p.sticky, session)
	}

	// Three tiers, cheapest first. Nothing in a later tier is touched while
	// anything in an earlier one can serve.
	best := p.leastLoaded(func(a *Account) bool {
		return a.eligible() && !a.OverThresholdFor(model, p.threshold()) && usable(a)
	})
	if best == nil {
		// Over-threshold but not yet rejected: the provider may still say
		// yes, and refusing locally turns a maybe into a certain failure.
		//
		// Excludes accounts that would bill. Without that, once every
		// account is over threshold this tier picks by load and can reach
		// for the paid one while a free account has not even been asked —
		// and a free account being over threshold costs nothing to try. All
		// free quota is spent before any money is.
		best = p.leastLoaded(func(a *Account) bool {
			return a.eligible() && !p.wouldBill(a) && usable(a)
		})
	}
	if best == nil {
		// Last tier: quota is gone, but the account has extra usage
		// available and the user has agreed to pay for it. Reached only when
		// every free option is spent — the alternative here is holding for
		// hours or failing outright, and this is the only tier that costs
		// money, so it must never be entered while anything above it works.
		//
		// The permission check is per account rather than a gate on the
		// whole tier: p.AllowOverage is only the default, and an account
		// with an explicit opt-in must work even when the pool says no.
		best = p.leastLoaded(func(a *Account) bool {
			return !a.disabledOrParked() && a.CanOverage(p.allowOverage) && usable(a)
		})
	}
	if best == nil {
		return nil
	}
	best.addInFlight(1)
	p.sticky[session] = best.Name
	if p.sessionProvider == nil {
		p.sessionProvider = map[string]string{}
	}
	if _, seen := p.sessionProvider[session]; !seen {
		p.sessionProvider[session] = ProviderOf(best.Type)
	}
	return best
}

// WhyUnavailable explains, for a request nothing could serve, whether the
// obstacle was capability rather than quota — so the client gets the real
// reason instead of a generic "all accounts exhausted".
func (p *Pool) WhyUnavailable(body []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if !a.eligible() {
			continue
		}
		if err := CanServe(a, body); err != nil {
			return err // an account was up and willing, but cannot serve this
		}
	}
	return nil
}

// leastLoaded picks the preferred account among those matching: lowest
// Priority first, then fewest in-flight. Priority is a preference rather
// than a queue — a higher-priority account that is exhausted or cannot serve
// the request never blocks a lower-priority one, because it fails `match`.
func (p *Pool) leastLoaded(match func(*Account) bool) *Account {
	var best *Account
	for _, a := range p.accounts {
		if !match(a) {
			continue
		}
		if best == nil || better(a, best) {
			best = a
		}
	}
	return best
}

func better(a, b *Account) bool {
	if pa, pb := a.Priority(), b.Priority(); pa != pb {
		return pa < pb
	}
	return a.load() < b.load()
}

// Done releases a request slot previously taken by Select.
func (p *Pool) Done(a *Account) { a.addInFlight(-1) }

// maxExhaustedHorizon caps how far into the future a single exhaustion can
// sentence an account, regardless of what a reset value claims (issue #90).
// spillway's longest legitimate provider window is the weekly bucket
// (7d/168h); this leaves generous slack above that for clock skew rather
// than clipping a real 7d-only rejection, while still bounding a corrupted
// or wildly-wrong reset (a bad epoch parse, a provider bug, a stale
// org-level cap reported far past when it actually lifts) to something a
// re-probe can still recover from, instead of trusting an arbitrary claimed
// duration outright.
const maxExhaustedHorizon = 9 * 24 * time.Hour

// maxProbeBackoff caps how far the re-probe spacing (probeBackoff) can grow
// for a single exhausted account (issue #90), so a genuinely long-spent
// account still gets checked at least this often instead of the backoff
// drifting toward "never."
const maxProbeBackoff = 24 * time.Hour

func capExhaustion(until time.Time) time.Time {
	if ceiling := time.Now().Add(maxExhaustedHorizon); until.After(ceiling) {
		return ceiling
	}
	return until
}

// MarkExhausted marks an account out of quota until the given time (§6.1:
// quota-429 rotates; the account is skipped until its window resets),
// capped at maxExhaustedHorizon (issue #90) rather than trusting an
// arbitrarily far-future reset outright.
func (p *Pool) MarkExhausted(a *Account, until time.Time) {
	a.mu.Lock()
	a.state = StateExhausted
	a.exhaustedUntil = capExhaustion(until)
	a.mu.Unlock()
}

// MarkReprobeRejected records that a re-probe of an already-exhausted
// account (issue #90) was rejected again: extends the bench to the fresh
// `until` (capped the same as MarkExhausted) and grows the probe backoff —
// baseInterval on the first rejected re-probe, doubling on every one after
// that, capped at maxProbeBackoff — rather than resetting it to baseInterval,
// so a genuinely spent account gets re-probed less and less often instead of
// every tick forever. The backoff is never reset to zero here; only
// ClearExhausted (a re-probe that actually recovers) does that.
func (p *Pool) MarkReprobeRejected(a *Account, until time.Time, baseInterval time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state = StateExhausted
	a.exhaustedUntil = capExhaustion(until)
	if a.probeBackoff < baseInterval {
		a.probeBackoff = baseInterval
	} else {
		a.probeBackoff *= 2
	}
	if a.probeBackoff > maxProbeBackoff {
		a.probeBackoff = maxProbeBackoff
	}
	a.nextProbeAt = time.Now().Add(a.probeBackoff)
}

// ClearExhausted un-benches an account a re-probe found healthy again
// (issue #90), rather than waiting for exhaustedUntil to arrive on its own:
// a rejection that turns out to have been transient, spurious, or misread
// (measured live: a restart found an account StateOK and immediately
// servable, with its weekly window reading 17% remaining, days before its
// recorded exhaustedUntil) should not sentence the account to its original
// deadline just because that deadline hasn't arrived yet. A no-op unless
// the account is currently exhausted, so it can never revive a disabled or
// parked account.
func (p *Pool) ClearExhausted(a *Account) {
	a.mu.Lock()
	wasExhausted := a.state == StateExhausted
	if wasExhausted {
		a.state = StateOK
		a.exhaustedUntil = time.Time{}
		a.probeBackoff = 0
		a.nextProbeAt = time.Time{}
	}
	a.mu.Unlock()
	if wasExhausted {
		// Issue #105: this is #90's bench-clears-early case — a held
		// request parked on the OLD (later) exhaustedUntil cannot see this
		// on its own. Signalled after releasing a.mu, never while holding
		// it: SelectExcept takes p.mu then an account's a.mu, so taking
		// them in the opposite order here (a.mu then p.mu, inside
		// SignalCapacity) would be a lock-order inversion.
		p.SignalCapacity()
	}
}

// MarkWindowRejected records that upstream has refused window `name` until
// `until` (issue #54's correction to #24): a hard, confirmed exclusion for
// whatever model that window governs, deliberately NOT touching a.state —
// a fable-only rejection must leave the account StateOK so Sonnet/Haiku
// requests (governed only by the account-wide "5h"/"7d" windows) keep
// serving from it.
//
// This also updates the visible QuotaWindow of the same name (utilization
// forced to 1.0, ResetAt set to until) so the admin/dashboard surface
// (accountJSON.FableSpent, driven by OverThresholdForWindow) reflects the
// rejection immediately, rather than waiting on whatever utilization value
// happened to arrive on the header — which issue #54 explicitly does not
// trust to already be at/above the switch threshold.
//
// The forged row carries until as its ResetAt, so it expires together with
// the windowRejected entry (issue #135) rather than outliving it: before
// windows expired at all, the map half of this cleared on the deadline and
// the row half kept the account fableSpent forever.
func (p *Pool) MarkWindowRejected(a *Account, name string, until time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.windowRejected == nil {
		a.windowRejected = map[string]time.Time{}
	}
	a.windowRejected[name] = until

	now := time.Now()
	for i := range a.windows {
		if a.windows[i].Name == name {
			a.windows[i].Used = 1
			a.windows[i].Limit = 1
			a.windows[i].ResetAt = until
			a.windows[i].Source = "headers"
			a.windows[i].FetchedAt = now
			return
		}
	}
	a.windows = append(a.windows, QuotaWindow{
		Name: name, Limit: 1, Used: 1, ResetAt: until, Source: "headers", FetchedAt: now,
	})
}

// RecordQuota stores the raw rate-limit headers for inspection and fills
// quota windows via the provider — which knows whether its quota arrives in
// headers at all (§6.20).
func (p *Pool) RecordQuota(a *Account, h http.Header, now time.Time) {
	q := map[string]string{}
	for k, v := range h {
		if strings.HasPrefix(strings.ToLower(k), "anthropic-ratelimit-") && len(v) > 0 {
			q[strings.ToLower(k)] = v[0]
		}
	}
	if len(q) > 0 {
		a.mu.Lock()
		a.quota = q
		a.quotaAt = now
		a.mu.Unlock()
	}
	spec := provider.For(a.Type)
	// Before the early returns below: overage is reported on every response,
	// including ones that carry no window data.
	if spec.OverageFromHeaders != nil {
		if ov := spec.OverageFromHeaders(h); ov.Known {
			a.setOverage(ov)
		}
	}
	if spec.WindowsFromHeaders == nil {
		return
	}
	pw := spec.WindowsFromHeaders(h, now)
	if len(pw) == 0 {
		return
	}
	out := make([]QuotaWindow, 0, len(pw))
	for _, w := range pw {
		out = append(out, QuotaWindow{
			Name: w.Name, Limit: w.Limit, Used: w.Used,
			ResetAt: w.ResetAt, Source: w.Source, FetchedAt: now,
		})
	}
	a.setWindowsSourced("headers", out)
}

// EffectiveModelMap is the account's own mapping over its provider's
// defaults.
//
// The account's entries win key by key rather than replacing the map: a user
// who maps one model should not lose the defaults for every other, which is
// what returning whichever map is non-empty would do.
//
// Nil when neither exists, which keeps the no-rewrite path for providers that
// speak the same ids we were given.
func (a *Account) EffectiveModelMap() map[string]string {
	base := provider.For(a.Type).DefaultModelMap
	if len(a.ModelMap) == 0 {
		return base
	}
	if len(base) == 0 {
		return a.ModelMap
	}
	out := make(map[string]string, len(base)+len(a.ModelMap))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range a.ModelMap {
		out[k] = v
	}
	return out
}

// setOverage records what the provider last said about extra usage.
func (a *Account) setOverage(ov provider.Overage) {
	a.mu.Lock()
	a.overage = ov
	a.mu.Unlock()
}

// Overage reports the last known extra-usage state. Known is false until a
// response has actually said so — an account that has never been used tells
// us nothing, and must not be assumed billable.
func (a *Account) Overage() provider.Overage {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.overage
}

// CanOverage reports whether this account may be used past its quota, at
// cost. Default is no — silently billing someone because their quota ran out
// is not a default any tool gets to choose.
//
// The three cases differ in who is asserting what:
//
//   - The provider said no. Believe it, whatever the config says: the request
//     would be rejected anyway, and arguing costs a round trip.
//   - The account is explicitly opted in. Try it, even with no confirming
//     header. This is not optional: spillway will not probe a spent account
//     precisely because that probe would be billed, so for the account that
//     most needs extra usage the header may never arrive. Requiring it would
//     leave the feature permanently unreachable on exactly the accounts it
//     exists for. The user naming one account is an assertion, not just a
//     permission.
//   - Only the pool-wide default is set. Then the user has singled nothing
//     out, so require the provider to have confirmed it.
func (a *Account) CanOverage(poolAllows bool) bool {
	a.mu.Lock()
	ov, allow := a.overage, a.allowOverage
	a.mu.Unlock()
	if ov.Known && !ov.Available {
		return false
	}
	if allow != nil {
		return *allow
	}
	return poolAllows && ov.Known && ov.Available
}

// Park removes an account from rotation by operator choice. Reversible, and
// distinct from the credential-death disable.
func (a *Account) Park() {
	a.mu.Lock()
	a.parked = true
	a.mu.Unlock()
}

// Unpark reverses Park. It does NOT clear a credential-death disable.
func (a *Account) Unpark() {
	a.mu.Lock()
	a.parked = false
	a.mu.Unlock()
}

// Parked reports an operator disable.
func (a *Account) Parked() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.parked
}

// Label returns the display name the dashboard uses instead of Name, or ""
// to fall back to deriving one from Name.
func (a *Account) Label() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.label
}

// SetLabel sets the display name. Locked: this is written from the admin
// settings handler while the selector and the dashboard's own read path can
// run concurrently (issue #13).
func (a *Account) SetLabel(l string) {
	a.mu.Lock()
	a.label = l
	a.mu.Unlock()
}

// Priority returns the account's selection priority (lower preferred).
func (a *Account) Priority() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.priority
}

// SetPriority sets the selection priority. Locked for the same reason as
// SetLabel — better() reads Priority from the selector goroutine while the
// dashboard can write it concurrently (issue #13).
func (a *Account) SetPriority(p int) {
	a.mu.Lock()
	a.priority = p
	a.mu.Unlock()
}

// SetAllowOverage sets the per-account overage override (nil = follow the
// pool default). Locked: CanOverage reads this under mu from the selector
// while the dashboard settings handler can write it concurrently — this is
// the money switch the issue is named for, so an unguarded write here is not
// acceptable at any point.
func (a *Account) SetAllowOverage(v *bool) {
	a.mu.Lock()
	a.allowOverage = v
	a.mu.Unlock()
}

// SetLastModel records the model this account most recently served.
func (a *Account) SetLastModel(m string) {
	a.mu.Lock()
	a.lastModel = m
	a.mu.Unlock()
}

// LastModel returns the model this account most recently served.
func (a *Account) LastModel() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastModel
}

// Quota returns the account's last recorded rate-limit headers and when.
func (a *Account) Quota() (map[string]string, time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.quota, a.quotaAt
}

// SetQuotaWindows stores provider quota state (§6.5), merging by window
// name (issue #100).
func (a *Account) SetQuotaWindows(w []QuotaWindow) {
	source := "poll"
	for _, x := range w {
		if x.Source != "" {
			source = x.Source
			break
		}
	}
	a.setWindowsSourced(source, w)
}

// setWindowsSourced merges an incoming batch of windows into the account's
// known set, keyed by Name (issue #100). A window present in w overwrites
// whatever was stored under that name, whichever source reported it before
// — the newest report always wins. A window NOT mentioned in w is left
// exactly as it was: absence in one response is not evidence the window
// went away, only that this response had nothing to say about it.
//
// This replaces an earlier design that replaced everything from one
// source wholesale. That was wrong the moment one source's own responses
// stopped being a complete picture: anthropic-ratelimit-unified-7d_oi-*
// (spillway's "7d-fable") only rides on a fable response, so a plain
// Sonnet/Opus/Haiku response — still source "headers" — carried just
// [5h, 7d] and the wholesale replace deleted the account's fable window
// on every non-fable turn (issue #100).
//
// It still stamps Source onto what it stores, and merging by Name rather
// than filtering by Source is also what keeps this immune to the earlier
// Kimi bug (a hundred and five windows in an afternoon, from a poller
// payload that set no Source: the old filter compared "poll" against ""
// and matched nothing, so every poll appended a fresh copy of every
// window instead of replacing it). Matching by Name has no equivalent
// failure mode — a poll that repeats the same names just overwrites the
// same entries, whatever Source ends up stamped on them.
func (a *Account) setWindowsSourced(source string, w []QuotaWindow) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range w {
		w[i].Source = source
	}
	incoming := make(map[string]bool, len(w))
	for _, nw := range w {
		incoming[nw.Name] = true
	}
	for _, nw := range w {
		replaced := false
		for i := range a.windows {
			if a.windows[i].Name == nw.Name {
				old := a.windows[i]
				a.windows[i] = nw
				replaced = true
				if turnedOver(old, nw) {
					a.retireCycleSiblingsLocked(old, nw, incoming)
				}
				break
			}
		}
		if !replaced {
			a.windows = append(a.windows, nw)
		}
	}
}

// Turnover detection (issue #135). Within one billing cycle a window's
// utilization only rises; a reading that has fallen to (near) zero from a
// materially higher one can only mean the cycle ended in between. The floor
// and the drop together keep a limit change from counting: Anthropic's "50%
// higher through <date>" boost lowers a utilization by a third, never to
// nothing, and a window that was barely used (old below the drop) proves
// nothing either way and falls through to plain reset expiry.
const (
	turnoverFloor = 0.05
	turnoverDrop  = 0.10
)

// turnedOver reports that nw is a fresh reading of old's window taken after
// old's cycle ended. Requires old to carry a reset: without one there is no
// cycle to relate any sibling to.
func turnedOver(old, nw QuotaWindow) bool {
	if old.Limit <= 0 || nw.Limit <= 0 || old.ResetAt.IsZero() {
		return false
	}
	before, after := old.Used/old.Limit, nw.Used/nw.Limit
	return after <= turnoverFloor && before-after >= turnoverDrop
}

// retireCycleSiblingsLocked ends the cycle of every stored window that shared
// old's reported reset and was not itself re-measured by this response
// (issue #135). Measured live, 2026-09-01: an account's 7d went 0.89 -> 0.0
// while the 7d-reset header stayed put at a value 31 hours ahead, and the
// 7d-fable window — same reported reset, only ever re-measured by a fable
// response, and the spent reading was what kept fable away — sat at 1.0
// until someone pinned the account and sent a fable request by hand. The
// provider's reset header lagged the turnover; the sibling's utilization did
// not.
//
// "Shared the reset" is the only link used: no family knowledge, no
// provider-specific pairing, just the provider's own statement that the two
// windows refill at the same moment. The retired window's ResetAt becomes
// the moment the turnover was observed — a measured time, not a guessed one
// (#90 is why resets are not guessed) — so currentAt reads it as expired and
// the next request for that family re-measures it. Assumes a.mu is held.
func (a *Account) retireCycleSiblingsLocked(old, nw QuotaWindow, incoming map[string]bool) {
	for i := range a.windows {
		x := &a.windows[i]
		if incoming[x.Name] || !x.ResetAt.Equal(old.ResetAt) || !x.FetchedAt.Before(nw.FetchedAt) {
			continue
		}
		x.ResetAt = nw.FetchedAt
	}
}

// QuotaWindows returns the latest provider quota state.
func (a *Account) QuotaWindows() []QuotaWindow {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := append([]QuotaWindow(nil), a.windows...)
	// Expired is stamped here for the same reason the sort is here: every
	// consumer reads through this one function. Assigned on every copy, not
	// set when true — a caller that round-trips a window it read back must
	// not be able to store a stale flag (issue #135).
	now := time.Now()
	for i := range out {
		out[i].Expired = !out[i].currentAt(now)
	}
	// Sorted here rather than at each display site (issue #106): the stored
	// order is whatever setWindowsSourced last produced, which depends on
	// arrival order and on which subset a response happened to carry, so it
	// differs between accounts on one page and shifts under the reader as
	// requests come in. Every consumer — dashboard tanks, the figures table,
	// the chart legend, the statusline — reads through here, so one sort
	// keeps them all agreeing.
	sort.Slice(out, func(i, j int) bool { return lessWindow(out[i].Name, out[j].Name) })
	return out
}

// OverThreshold reports whether ANY quota window is at/above the used
// fraction (§6.5 predictive rotation: skip this account while another
// eligible one exists). A window whose reset has passed no longer counts
// (issue #135, QuotaWindow.currentAt).
func (a *Account) OverThreshold(frac float64) bool {
	return a.overThresholdAt(frac, time.Now())
}

// overThresholdAt is OverThreshold against an explicit clock, for tests.
// Unlike the package's other lowercase twins (threshold, wouldBill) it is
// NOT "assumes the lock is held": it takes a.mu itself, exactly as the
// exported form did. The suffix marks the injected clock, nothing else.
func (a *Account) overThresholdAt(frac float64, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, w := range a.windows {
		if !w.currentAt(now) {
			continue
		}
		if w.Limit > 0 && w.Used/w.Limit >= frac {
			return true
		}
	}
	return false
}

// EarliestQuotaReset reports the soonest known quota reset, false when no
// window carries one — used to bound exhaustion when a provider 429/403
// doesn't say when quota returns.
func (a *Account) EarliestQuotaReset() (time.Time, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var earliest time.Time
	ok := false
	for _, w := range a.windows {
		if w.ResetAt.IsZero() {
			continue
		}
		if !ok || w.ResetAt.Before(earliest) {
			earliest = w.ResetAt
			ok = true
		}
	}
	return earliest, ok
}

// State reports the account's current state; an exhausted account whose
// window has passed flips back to StateOK.
func (a *Account) State() State {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stateLocked()
}

// ExhaustedUntil reports when an exhausted account becomes eligible again.
func (a *Account) ExhaustedUntil() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.exhaustedUntil
}

// NextProbeAt reports the earliest time a re-probe of this account should
// run again, following a rejected re-probe's backoff (issue #90). Zero
// means no extra restriction beyond the caller's own staleness check.
func (a *Account) NextProbeAt() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.nextProbeAt
}

func (a *Account) stateLocked() State {
	if a.state == StateExhausted && time.Now().After(a.exhaustedUntil) {
		a.state = StateOK
	}
	return a.state
}

func (a *Account) eligible() bool { return !a.Parked() && a.State() == StateOK }

// WouldBill reports that using this account now would be charged: its quota
// is gone (spent or rejected) and extra usage would cover the request.
//
// Exported because the proxy has to answer the same question after selection
// and must get the same answer. Inferring it from StateExhausted instead
// misses the commonest case by far — an account whose quota headers say it is
// finished but which has never been 429'd, which is exactly what a spent
// account looks like when spillway has been rotating away from it correctly.
// Locked: unlike wouldBill (its unexported counterpart, called only from
// within SelectExcept which already holds mu), a caller outside the package
// has no other synchronization with Apply's write to allowOverage.
func (p *Pool) WouldBill(a *Account) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.wouldBill(a)
}

// wouldBill assumes mu is already held.
func (p *Pool) wouldBill(a *Account) bool {
	if !a.CanOverage(p.allowOverage) {
		return false
	}
	return a.State() == StateExhausted || a.OverThreshold(p.threshold())
}

// AllowOverage reports the pool-wide default for serving from an account
// past its subscription quota, at cost (per-account CanOverage can still
// override it). Locked: this is exactly the field the dashboard settings
// handler used to overwrite unguarded from the admin HTTP goroutine
// (issue #13) while this getter's callers (e.g. the dashboard's own account
// listing) ran concurrently with the selector.
func (p *Pool) AllowOverage() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.allowOverage
}

// HideOverageFromClient reports whether pooled Claude responses have their
// credit markers removed before reaching the client (issue #103). Read by
// the proxy per response rather than snapshotted at construction, so a
// settings change applies to the next request like every pool setting.
func (p *Pool) HideOverageFromClient() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hideOverageFromClient
}

// disabledOrParked is the overage tier's floor. That tier deliberately admits
// StateExhausted accounts — being out of quota is the precondition for using
// extra usage at all — but a dead credential or an operator park still means
// no.
func (a *Account) disabledOrParked() bool {
	return a.Parked() || a.State() == StateDisabled
}

func (a *Account) load() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.inFlight
}

// InFlight reports the account's current in-flight request count.
func (a *Account) InFlight() int { return a.load() }

func (a *Account) addInFlight(d int) {
	a.mu.Lock()
	a.inFlight += d
	a.mu.Unlock()
}

func (p *Pool) find(name string) *Account {
	for _, a := range p.accounts {
		if a.Name == name {
			return a
		}
	}
	return nil
}

// Remove takes name out of the pool immediately (issue #83): the very next
// SelectExcept call can no longer choose it, and every background loop that
// walks Accounts() (the refresh sweep, idle probing, the canary, quota
// snapshotting) stops touching it from its next tick. Reports whether an
// account by that name was actually present.
//
// A request already in flight on this account is deliberately left alone —
// Remove drains rather than aborts. SelectExcept handed that request's
// caller the *Account pointer directly, not a name to look up every time, so
// it keeps working until it finishes and calls Done on that same pointer,
// exactly as Park already leaves in-flight work alone for a disabled
// account. This matters more here than for Park: the credential backing
// this account has usually just been deleted from the secret store by the
// caller (that is the whole bug #83 is about), but the *Account already
// holds whatever access token it read into memory before that happened, so
// a request mid-flight is unaffected either way. Aborting it instead would
// turn a clean removal into a failed request for no benefit — nothing
// downstream needs the removal to be instantaneous for requests already
// under way, only for requests that have not started yet.
func (p *Pool) Remove(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, a := range p.accounts {
		if a.Name != name {
			continue
		}
		// A fresh backing array, not a reslice of the old one: Accounts()
		// used to hand out p.accounts directly, and even now that it copies
		// under lock, leastLoaded/WhyUnavailable/find all range over
		// p.accounts while holding mu — a append(p.accounts[:i], ...)
		// in-place splice would be visible to nothing outside this lock, but
		// there is no reason to rely on that when the allocation is cheap
		// and removal is not a hot path.
		next := make([]*Account, 0, len(p.accounts)-1)
		next = append(next, p.accounts[:i]...)
		next = append(next, p.accounts[i+1:]...)
		p.accounts = next
		if p.pinned == name {
			p.pinned = ""
		}
		// Sessions sticky to this account self-heal on their next
		// SelectExcept call: find(name) returns nil, so the sticky entry is
		// dropped and selection falls through to the ordinary tiers. No
		// need to walk p.sticky/p.sessionProvider here.
		return true
	}
	return false
}

// Add puts a into the pool immediately (issue #87, mirroring Remove's
// locking discipline from #85/#83): the very next SelectExcept call can
// choose it, and every background loop that walks Accounts() (the refresh
// sweep, idle probing, the canary, quota snapshotting) picks it up from its
// next tick. Reports whether it was actually added — false when an account
// by that name already exists, so a caller cannot silently end up with two
// entries answering to the same name (selection, Remove-by-name and Pin all
// assume names are unique). A re-authenticated existing account is not
// Add's job: the caller should update that *Account's credentials in place
// (Account.SetCredentials) instead, which also revives it from
// StateDisabled — see admin's account-add handler.
func (p *Pool) Add(a *Account) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, existing := range p.accounts {
		if existing.Name == a.Name {
			return false
		}
	}
	// Append rather than mutate a shared backing array in place: Accounts()
	// copies out under lock (see its own comment), so aliasing would be
	// safe today, but Remove already established the pattern of not
	// relying on that — see its comment for why.
	next := make([]*Account, len(p.accounts)+1)
	copy(next, p.accounts)
	next[len(p.accounts)] = a
	p.accounts = next
	// Issue #105: a newly-added account is exactly the transition a
	// currently-parked request cannot otherwise see — wake it rather than
	// leaving it to find out only when its timer eventually fires.
	p.signalCapacityLocked()
	return true
}

// SetOverageForTest seeds the extra-usage state. Production code learns this
// only from a provider response; tests need to arrange the state that a
// response would have produced.
func (a *Account) SetOverageForTest(ov provider.Overage) { a.setOverage(ov) }

// AccountSettings is the subset of one account's config that the dashboard
// can edit live. A plain DTO rather than *config.AccountConfig so this
// package doesn't have to import internal/config — pool has no dependency on
// config today (config doesn't import pool either, so nothing would cycle),
// but pulling in the whole on-disk config schema for four fields would
// couple pool's account model to yaml tags, defaults and validation that
// belong to config, not to selection. The caller (cmd/spillway) does the
// mapping from *config.Config.
type AccountSettings struct {
	// Name identifies the pool account this applies to; entries whose Name
	// doesn't match any pool account are ignored.
	Name         string
	Disabled     bool
	Label        string
	Priority     int
	AllowOverage *bool
}

// Settings is the subset of pool-wide config the dashboard can edit live,
// plus any per-account settings to apply alongside it. See AccountSettings
// for why this isn't *config.Config.
type Settings struct {
	SwitchThreshold float64
	CrossProvider   bool
	AllowOverage    bool
	// StickyAcrossFamily is pool.stickyAcrossFamily's config-facing value
	// (issue #24 decision 2). See that field's comment.
	StickyAcrossFamily bool
	// HideOverageFromClient is pool.hideOverageFromClient's config-facing
	// value (issue #103). See that field's comment.
	HideOverageFromClient bool
	// Accounts, if non-empty, is applied to the matching pool accounts by
	// Name. Left empty (e.g. from buildPool at startup, where per-account
	// fields are already set at construction and park state is applied
	// separately) it touches no account — Apply only ever parks/unparks or
	// relabels an account explicitly named here, it never unparks one just
	// because it's absent from this call.
	Accounts []AccountSettings
}

// Apply sets pool-wide and named per-account settings atomically with
// respect to Select/SelectFor/SelectExcept.
//
// This replaces the dashboard settings handler assigning SwitchThreshold,
// CrossProvider, AllowOverage, Label, Priority and AllowOverage as plain
// struct fields directly from the admin HTTP goroutine (issue #13): every
// one of those is read by the selector (SelectExcept, better, CanOverage)
// from a request-handling goroutine, under mu, so a plain field write raced
// with it — for AllowOverage, a torn read of the money switch mid-selection
// could decide whether a request bills. Apply is now the only writer, and
// takes the same locks the selector's reads do.
func (p *Pool) Apply(s Settings) {
	p.mu.Lock()
	p.switchThreshold = s.SwitchThreshold
	p.crossProvider = s.CrossProvider
	p.allowOverage = s.AllowOverage
	p.stickyAcrossFamily = s.StickyAcrossFamily
	p.hideOverageFromClient = s.HideOverageFromClient
	p.mu.Unlock()

	if len(s.Accounts) == 0 {
		return
	}
	byName := make(map[string]AccountSettings, len(s.Accounts))
	for _, as := range s.Accounts {
		byName[as.Name] = as
	}
	for _, a := range p.Accounts() {
		as, ok := byName[a.Name]
		if !ok {
			continue
		}
		// Park/Unpark, not Disable/Enable: un-parking must never revive an
		// account whose credential died.
		if as.Disabled {
			a.Park()
		} else {
			wasParked := a.Parked()
			a.Unpark()
			if wasParked {
				// Issue #105: an operator un-parking an account is a
				// transition into potentially-usable capacity, same as
				// Add — a held request should not have to wait out its
				// timer to notice.
				p.SignalCapacity()
			}
		}
		a.SetLabel(as.Label)
		a.SetPriority(as.Priority)
		a.SetAllowOverage(as.AllowOverage)
	}
}
