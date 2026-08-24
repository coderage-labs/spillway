package main

import (
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/coderage-labs/spillway/internal/admin"
	"github.com/coderage-labs/spillway/internal/config"
	"github.com/coderage-labs/spillway/internal/events"
	"github.com/coderage-labs/spillway/internal/pool"
)

func win(name string, used, limit float64) slWindow {
	return slWindow{Name: name, Used: used, Limit: limit}
}

func TestHeadroomClampsAndReportsUnknown(t *testing.T) {
	for _, tc := range []struct {
		w    slWindow
		want float64
	}{
		{win("5h", 0, 1), 1},
		{win("5h", 0.42, 1), 0.58},
		{win("5h", 1, 1), 0},
		{win("5h", 2, 1), 0},  // over-reported usage clamps, never negative
		{win("5h", 0, 0), -1}, // no limit reported: unknown, not full
		{win("5h", -1, 1), 1}, // clamps the other way too
	} {
		// Tolerance, not equality: 1-0.42 is 0.5800000000000001 in float64.
		if got := tc.w.headroom(); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("headroom(used=%v limit=%v) = %v, want %v", tc.w.Used, tc.w.Limit, got, tc.want)
		}
	}
}

// A live window must never render as a completely empty bar — that reads as
// "dry" when there is still headroom left.
func TestBarNeverEmptyWhileWindowIsLive(t *testing.T) {
	out := bar(noColour, 0.001)
	if !strings.Contains(out, "█") {
		t.Errorf("tiny-but-live headroom rendered with no filled cell: %q", out)
	}
	if full := bar(noColour, 1); strings.Contains(full, "░") {
		t.Errorf("full headroom should have no empty cells: %q", full)
	}
	if empty := bar(noColour, 0); strings.Contains(empty, "█") {
		t.Errorf("zero headroom should have no filled cells: %q", empty)
	}
}

func TestBarWidthIsConstant(t *testing.T) {
	// Constant width matters: the status line sits next to other segments.
	for _, h := range []float64{0, 0.13, 0.5, 0.99, 1} {
		if n := len([]rune(bar(noColour, h))); n != barCells {
			t.Errorf("bar(%v) is %d cells, want %d", h, n, barCells)
		}
	}
}

func TestNoColourEmitsNoEscapes(t *testing.T) {
	out := bar(noColour, 0.5) + noColour.dim("x") + noColour.paint(gradeFor(0), "y")
	if strings.Contains(out, "\033") {
		t.Errorf("NO_COLOR output contains escape sequences: %q", out)
	}
}

// Whoever is serving wins; otherwise the account the next request would land
// on — which is the one with the most headroom in its tightest window.
func TestPickAccountPrefersServingThenHealthiest(t *testing.T) {
	serving := slAccount{Name: "busy", State: "ok", InFlight: 1}
	rich := slAccount{Name: "rich", State: "ok", Windows: []slWindow{win("5h", 0.1, 1)}}
	poor := slAccount{Name: "poor", State: "ok", Windows: []slWindow{win("5h", 0.9, 1)}}

	if got := pickAccount([]slAccount{rich, serving, poor}); got.Name != "busy" {
		t.Errorf("serving account should win, got %q", got.Name)
	}
	if got := pickAccount([]slAccount{poor, rich}); got.Name != "rich" {
		t.Errorf("healthiest should win, got %q", got.Name)
	}
	// An account is only as good as its WORST window.
	mixed := slAccount{Name: "mixed", State: "ok",
		Windows: []slWindow{win("5h", 0, 1), win("7d", 0.95, 1)}}
	if got := pickAccount([]slAccount{mixed, poor}); got.Name != "poor" {
		t.Errorf("a spent weekly window should sink the account, got %q", got.Name)
	}
}

func TestPickAccountSkipsUnusableAndReportsNone(t *testing.T) {
	dry := slAccount{Name: "dry", State: "exhausted", Windows: []slWindow{win("5h", 1, 1)}}
	off := slAccount{Name: "off", State: "disabled"}
	if got := pickAccount([]slAccount{dry, off}); got != nil {
		t.Errorf("no usable account should return nil, got %q", got.Name)
	}
}

func TestDisplayPrefersLabelThenDomain(t *testing.T) {
	for _, tc := range []struct {
		a    slAccount
		want string
	}{
		{slAccount{Name: "you@acme.example", Label: "work"}, "work"},
		// Domain, not local part: two accounts often share a local part.
		{slAccount{Name: "you@acme.example"}, "acme"},
		{slAccount{Name: "kimi"}, "kimi"},
	} {
		if got := tc.a.display(); got != tc.want {
			t.Errorf("display(%+v) = %q, want %q", tc.a, got, tc.want)
		}
	}
}

func TestShortModel(t *testing.T) {
	for in, want := range map[string]string{
		"claude-haiku-4-5-20251001": "haiku-4-5",
		"claude-sonnet-4-6":         "sonnet-4-6",
		"k3":                        "k3",
		"kimi-for-coding":           "kimi-for-coding",
	} {
		if got := shortModel(in); got != want {
			t.Errorf("shortModel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsOursRecognisesOnlySpillway(t *testing.T) {
	for cmd, want := range map[string]bool{
		"/Users/x/go/bin/spillway statusline": true,
		"spillway statusline":                 true,
		"~/.claude/statusline.sh":             false,
		"npx cc-statusline":                   false,
		"":                                    false,
	} {
		if got := isOurs(cmd); got != want {
			t.Errorf("isOurs(%q) = %v, want %v", cmd, got, want)
		}
	}
}

// ── what the line says when things go wrong ──────────────────────────────

func okAccount() slAccount {
	return slAccount{
		Name: "me@work.com", State: "ok", LastModel: "claude-haiku-4-5",
		Windows: []slWindow{{Name: "5h", Limit: 100, Used: 6, ResetAt: time.Now().Add(2 * time.Hour)}},
	}
}

// A parked request is the whole status. Showing 94% headroom while the user
// waits two hours for a reset would be worse than showing nothing.
func TestRenderHoldReplacesTheHeadroomLine(t *testing.T) {
	until := time.Now().Add(2*time.Hour + 14*time.Minute)
	st := slState{Holding: &slHold{Count: 1, Until: until}, Total: 1}
	got := render(noColour, []slAccount{okAccount()}, st, time.Now())

	if !strings.Contains(got, "held") {
		t.Errorf("%q does not say the request is held", got)
	}
	if !strings.Contains(got, "2h14m") {
		t.Errorf("%q does not say how long is left", got)
	}
	if !strings.Contains(got, until.Local().Format("15:04")) {
		t.Errorf("%q does not say when it lifts", got)
	}
	// The bars would imply the request could proceed.
	if strings.Contains(got, "█") || strings.Contains(got, "94%") {
		t.Errorf("%q still shows headroom while the request is parked", got)
	}
}

func TestRenderHoldShowsCountWhenSeveralAreWaiting(t *testing.T) {
	st := slState{Holding: &slHold{Count: 3, Until: time.Now().Add(time.Hour)}}
	if got := render(noColour, []slAccount{okAccount()}, st, time.Now()); !strings.Contains(got, "×3") {
		t.Errorf("%q does not show that 3 requests are parked", got)
	}
	one := slState{Holding: &slHold{Count: 1, Until: time.Now().Add(time.Hour)}}
	if got := render(noColour, []slAccount{okAccount()}, one, time.Now()); strings.Contains(got, "×1") {
		t.Errorf("%q clutters the single-request case with a count", got)
	}
}

// Dry with no hold (fail mode): still say when it comes back if we know.
func TestRenderDryPoolNamesTheReset(t *testing.T) {
	reset := time.Now().Add(45 * time.Minute)
	dead := slAccount{Name: "me@work.com", State: "exhausted"}
	st := slState{Total: 1, Exhausted: 1, NextReset: &reset}

	got := render(noColour, []slAccount{dead}, st, time.Now())
	if !strings.Contains(got, "pool dry") {
		t.Errorf("%q does not say the pool is dry", got)
	}
	if !strings.Contains(got, reset.Local().Format("15:04")) || !strings.Contains(got, "45m") {
		t.Errorf("%q does not say when the pool returns", got)
	}
}

// The states a human has to clear must be spelled out, not implied by a
// missing account.
func TestRenderNamesAccountsNeedingAttention(t *testing.T) {
	st := slState{Total: 4, Usable: 1, Exhausted: 1, Parked: 1, Disabled: 1}
	got := render(noColour, []slAccount{okAccount()}, st, time.Now())

	for _, want := range []string{"1 spent", "1 paused", "needs login"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q does not mention %q", got, want)
		}
	}
	// A healthy pool must stay quiet — this is a status line, not a report.
	clean := render(noColour, []slAccount{okAccount()}, slState{Total: 1, Usable: 1}, time.Now())
	for _, unwanted := range []string{"spent", "paused", "needs login"} {
		if strings.Contains(clean, unwanted) {
			t.Errorf("healthy pool line %q mentions %q", clean, unwanted)
		}
	}
}

// Issue #66: a genuine MITM CA regeneration that has left a client stranded
// must show a warning that names the fix outright — there is nothing else
// to say ("restart this session" is the whole remedy).
func TestRenderShowsStaleCAWarning(t *testing.T) {
	st := slState{Total: 1, Usable: 1, StaleCA: true}
	got := render(noColour, []slAccount{okAccount()}, st, time.Now())
	for _, want := range []string{"stale CA", "restart"} {
		if !strings.Contains(strings.ToLower(got), strings.ToLower(want)) {
			t.Errorf("%q does not mention %q", got, want)
		}
	}
}

// The commonest case by far — no regeneration has ever happened, or it
// happened but decayed back to healthy — must stay exactly as quiet as any
// other healthy pool.
func TestRenderOmitsStaleCAWarningWhenFalse(t *testing.T) {
	got := render(noColour, []slAccount{okAccount()}, slState{Total: 1, Usable: 1, StaleCA: false}, time.Now())
	if strings.Contains(strings.ToLower(got), "stale") {
		t.Errorf("%q mentions a stale CA warning with StaleCA=false", got)
	}
}

// The warning must show on every branch render() has — held, dry, and the
// ordinary account line — not just the common case, since a regeneration
// can strand a session regardless of what else the pool is doing.
func TestRenderShowsStaleCAWarningOnEveryBranch(t *testing.T) {
	now := time.Now()
	holding := render(noColour, nil, slState{StaleCA: true, Holding: &slHold{Count: 1, Until: now.Add(time.Minute)}}, now)
	dry := render(noColour, []slAccount{}, slState{StaleCA: true}, now)
	ordinary := render(noColour, []slAccount{okAccount()}, slState{Total: 1, Usable: 1, StaleCA: true}, now)
	for name, line := range map[string]string{"holding": holding, "dry": dry, "ordinary": ordinary} {
		if !strings.Contains(strings.ToLower(line), "stale") {
			t.Errorf("%s branch %q does not show the stale-CA warning", name, line)
		}
	}
}

// Once a window is nearly gone the percentage stops being the useful number.
func TestRenderShowsResetCountdownOnlyWhenLow(t *testing.T) {
	low := okAccount()
	low.Windows = []slWindow{{Name: "5h", Limit: 100, Used: 88, ResetAt: time.Now().Add(72 * time.Minute)}}
	got := render(noColour, []slAccount{low}, slState{Total: 1, Usable: 1}, time.Now())
	if !strings.Contains(got, "1h12m") {
		t.Errorf("%q does not say when the nearly-spent window resets", got)
	}

	// A healthy window does not need it, and the space is scarce.
	full := render(noColour, []slAccount{okAccount()}, slState{Total: 1, Usable: 1}, time.Now())
	if strings.Contains(full, "↻") {
		t.Errorf("%q shows a countdown on a window with 94%% left", full)
	}
}

// An older daemon has no /api/state; the zero value must not invent a
// degraded pool out of nothing.
func TestRenderToleratesMissingState(t *testing.T) {
	got := render(noColour, []slAccount{okAccount()}, slState{}, time.Now())
	if !strings.Contains(got, "94%") {
		t.Errorf("%q lost the headroom it could still show", got)
	}
	for _, unwanted := range []string{"held", "pool dry", "spent", "paused", "needs login"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("%q invented %q from an empty state response", got, unwanted)
		}
	}
}

func TestCompactDur(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{-time.Hour, "0s"},
		{30 * time.Second, "30s"},
		{89 * time.Second, "1m"},
		{90 * time.Second, "2m"},
		{45 * time.Minute, "45m"},
		{2 * time.Hour, "2h"},
		{2*time.Hour + 14*time.Minute, "2h14m"},
		{26 * time.Hour, "1d2h"},
		{48 * time.Hour, "2d"},
	}
	for _, c := range cases {
		if got := compactDur(c.d); got != c.want {
			t.Errorf("compactDur(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

// The status line decodes what the admin server encodes. Nothing in the
// compiler connects the two, so a renamed json tag on either side would show
// up as a status line that silently never reports a hold — the exact failure
// the hold display exists to prevent. Decode the real response.
func TestStatuslineDecodesTheRealStateResponse(t *testing.T) {
	spent := pool.NewAccount("spent", pool.SourceYAML, "tok", "", 0, "")
	paused := pool.NewAccount("paused", pool.SourceYAML, "tok", "", 0, "")
	p := pool.New([]*pool.Account{spent, paused}, time.Now())
	reset := time.Now().Add(time.Hour).Truncate(time.Second)
	p.MarkExhausted(spent, reset)
	paused.Park()
	release := p.BeginHold(reset)
	defer release()

	srv := httptest.NewServer(admin.New(p, nil, events.New(), ""))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	var st slState
	if err := getJSON(addr, "", "/api/state", &st); err != nil {
		t.Fatal(err)
	}
	if st.Holding == nil || st.Holding.Count != 1 || !st.Holding.Until.Equal(reset) {
		t.Fatalf("holding decoded as %+v, want 1 until %v", st.Holding, reset)
	}
	if st.Total != 2 || st.Exhausted != 1 || st.Parked != 1 || st.Usable != 0 {
		t.Errorf("counts decoded as total %d, exhausted %d, parked %d, usable %d",
			st.Total, st.Exhausted, st.Parked, st.Usable)
	}
	if st.NextReset == nil || !st.NextReset.Equal(reset) {
		t.Errorf("nextReset decoded as %v, want %v", st.NextReset, reset)
	}

	var accts []slAccount
	if err := getJSON(addr, "", "/api/accounts", &accts); err != nil {
		t.Fatal(err)
	}
	if len(accts) != 2 {
		t.Fatalf("decoded %d accounts, want 2", len(accts))
	}

	// And the whole thing renders as the hold, not as a healthy pool.
	line := render(noColour, accts, st, time.Now())
	if !strings.Contains(line, "held") || !strings.Contains(line, "1 paused") {
		t.Errorf("line = %q, want the hold and the paused account", line)
	}
}

// TestStatuslineDecodesStaleCAFromTheRealAdminServer: issue #66's staleCA
// field, wired the way main.go wires it (admin.Server.SetCAWarning), round
// trips through the real /api/state JSON encoding — not just a hand-built
// fixture — and shows up in the rendered line.
func TestStatuslineDecodesStaleCAFromTheRealAdminServer(t *testing.T) {
	healthy := pool.NewAccount("healthy", pool.SourceYAML, "tok", "", 0, "")
	p := pool.New([]*pool.Account{healthy}, time.Now())

	adminSrv := admin.New(p, nil, events.New(), "")
	adminSrv.SetCAWarning(func() bool { return true })
	srv := httptest.NewServer(adminSrv)
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	var st slState
	if err := getJSON(addr, "", "/api/state", &st); err != nil {
		t.Fatal(err)
	}
	if !st.StaleCA {
		t.Fatal("staleCA did not decode as true from the real admin server")
	}

	var accts []slAccount
	if err := getJSON(addr, "", "/api/accounts", &accts); err != nil {
		t.Fatal(err)
	}
	line := render(noColour, accts, st, time.Now())
	if !strings.Contains(strings.ToLower(line), "stale") {
		t.Errorf("line = %q, want the stale-CA warning", line)
	}
}

// An account whose own quota headers say it is finished is one the selector
// avoids, even though nothing has 429'd it. Reporting it as healthy was the
// bug a real exhausted account exposed: it sat at 100% of its 7-day
// window showing a full tank and "ok".
func TestRenderCountsAReserveAccountAsSpent(t *testing.T) {
	st := slState{Total: 2, Usable: 1, Reserve: 1}
	got := render(noColour, []slAccount{okAccount()}, st, time.Now())
	if !strings.Contains(got, "1 spent") {
		t.Errorf("%q does not report the over-threshold account as spent", got)
	}
	if strings.Contains(got, "pool dry") {
		t.Errorf("%q calls the pool dry while an account can still serve", got)
	}
}

// ...but a reserve account still serves, so the pool is not dry. Saying "dry"
// while requests succeed would be a worse lie than the one being fixed.
func TestRenderReserveOnlyPoolIsNotDry(t *testing.T) {
	low := okAccount()
	low.Windows = []slWindow{{Name: "7d", Limit: 100, Used: 99, ResetAt: time.Now().Add(3 * time.Hour)}}
	st := slState{Total: 1, Usable: 0, Reserve: 1}
	got := render(noColour, []slAccount{low}, st, time.Now())
	if strings.Contains(got, "pool dry") {
		t.Errorf("%q calls the pool dry with a reserve account still serving", got)
	}
	if !strings.Contains(got, "1 spent") {
		t.Errorf("%q does not warn that the last account is on its reserve", got)
	}
}

// selfPath must not resolve symlinks.
//
// Homebrew installs /opt/homebrew/bin/spillway as a symlink to a VERSIONED
// path under Caskroom, which the next upgrade deletes. Both the launchd job
// and the Claude Code status line record this path once, at install time, so
// resolving through the symlink breaks them at the next release — silently,
// and much later than the change that caused it.
func TestSelfPathKeepsTheSymlinkItWasInvokedAs(t *testing.T) {
	dir := t.TempDir()
	// Windows LookPath honours PATHEXT, so a file with no extension is not
	// executable there and the lookup falls through to os.Executable — which
	// made this fail on the runner while the behaviour it tests was correct.
	real := filepath.Join(dir, "versioned", exeName("spillway"))
	if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "bin", exeName("spillway"))
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	// Invoked by absolute path through the symlink, as a shell would.
	old := os.Args
	t.Cleanup(func() { os.Args = old })
	os.Args = []string{link}

	got, err := selfPath()
	if err != nil {
		t.Fatal(err)
	}
	if got != link {
		t.Errorf("selfPath() = %q, want the symlink %q — recording the target "+
			"bakes in a path the next upgrade removes", got, link)
	}
}

// A bare name is resolved on PATH, since the status line runs with a minimal
// environment where a relative command is not findable later.
func TestSelfPathResolvesABareNameOnPath(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, exeName("spillway-test-bin"))
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	old := os.Args
	t.Cleanup(func() { os.Args = old })
	os.Args = []string{exeName("spillway-test-bin")}

	got, err := selfPath()
	if err != nil {
		t.Fatal(err)
	}
	if got != bin {
		t.Errorf("selfPath() = %q, want the absolute %q", got, bin)
	}
}

// exeName gives a fixture the extension its platform needs to be executable.
func exeName(n string) string {
	if runtime.GOOS == "windows" {
		return n + ".exe"
	}
	return n
}

// The status line is installed once and then runs for every Claude Code
// session on the machine, including ones started without `spillway run`.
// Reporting the pool to those is worse than silence: the numbers are real,
// but they describe traffic the session is not part of, and the line reads as
// "you are on spillway" when you are not.
func TestAttachedToSpillway(t *testing.T) {
	cfg := config.Defaults()
	cfg.Proxy.Host, cfg.Proxy.Port = "127.0.0.1", 7654

	for _, tc := range []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"nothing set", nil, false},
		{"HTTPS_PROXY at us", map[string]string{"HTTPS_PROXY": "http://127.0.0.1:7654"}, true},
		{"lowercase https_proxy", map[string]string{"https_proxy": "http://127.0.0.1:7654"}, true},
		{"trailing slash", map[string]string{"HTTPS_PROXY": "http://127.0.0.1:7654/"}, true},
		{"no scheme", map[string]string{"HTTPS_PROXY": "127.0.0.1:7654"}, true},
		{"base URL route", map[string]string{"ANTHROPIC_BASE_URL": "http://127.0.0.1:7654"}, true},
		// Somebody else's proxy is the case this exists for: a corporate
		// proxy in the environment must not light up spillway's line.
		{"a different proxy", map[string]string{"HTTPS_PROXY": "http://proxy.corp:8080"}, false},
		{"right host, wrong port", map[string]string{"HTTPS_PROXY": "http://127.0.0.1:9999"}, false},
		{"empty value", map[string]string{"HTTPS_PROXY": ""}, false},
		{"garbage", map[string]string{"HTTPS_PROXY": "::::"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{"HTTPS_PROXY", "https_proxy", "ANTHROPIC_BASE_URL"} {
				t.Setenv(k, "")
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if got := attachedToSpillway(&cfg); got != tc.want {
				t.Errorf("attachedToSpillway = %v, want %v", got, tc.want)
			}
		})
	}
}

// A non-default listener has to be honoured, or anyone who moved the port
// gets a permanently blank line.
func TestAttachedHonoursAConfiguredPort(t *testing.T) {
	cfg := config.Defaults()
	cfg.Proxy.Port = 9100
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9100")
	if !attachedToSpillway(&cfg) {
		t.Error("did not recognise the configured port")
	}
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:7654")
	if attachedToSpillway(&cfg) {
		t.Error("matched the default port while configured for another")
	}
}

// A leading flag is not a subcommand. `statusline --always` printed
// `unknown statusline action "--always"` straight into the prompt.
func TestStatuslineFlagIsNotASubcommand(t *testing.T) {
	t.Setenv("SPILLWAY_CONFIG", filepath.Join(t.TempDir(), "spillway.yaml"))
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "ANTHROPIC_BASE_URL"} {
		t.Setenv(k, "")
	}
	// No daemon to reach, so this renders nothing either way; what matters is
	// that it is not an error.
	if err := runStatusline([]string{"--always"}); err != nil {
		t.Errorf("--always treated as an action: %v", err)
	}
	if err := runStatusline([]string{"instal"}); err == nil {
		t.Error("a mistyped subcommand should still be an error")
	}
}

// Issue #66's stale-CA warning must obey the statusline's existing rules
// exactly like every other signal on it: nothing for an unproxied session,
// and nothing when the daemon can't be reached — even while the warning
// itself is actively true. A hot warning must never be the thing that
// breaks those two silences.

// TestStatuslinePrintsNothingForUnproxiedSessionEvenWithStaleCA: the daemon
// is up and genuinely reporting staleCA=true, but this process's own
// environment says it isn't going through spillway. The statusline must
// still print nothing at all — reporting a warning that belongs to traffic
// this session isn't part of would be worse than the missing HTTPS_PROXY
// case ordinary rendering already guards against.
func TestStatuslinePrintsNothingForUnproxiedSessionEvenWithStaleCA(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/state":
			io.WriteString(w, `{"usable":1,"total":1,"staleCA":true}`)
		case "/api/accounts":
			io.WriteString(w, `[{"name":"a","state":"ok"}]`)
		}
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "spillway.yaml")
	if err := os.WriteFile(cfgPath, []byte("admin:\n  addr: "+addr+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPILLWAY_CONFIG", cfgPath)
	// Deliberately NOT proxied: nothing points at spillway's proxy address.
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "ANTHROPIC_BASE_URL"} {
		t.Setenv(k, "")
	}

	got := captureStdout(t, func() {
		if err := runStatusline(nil); err != nil {
			t.Errorf("runStatusline: %v", err)
		}
	})
	if got != "" {
		t.Errorf("unproxied session printed %q, want nothing even though the daemon reports staleCA=true", got)
	}
}

// TestStatuslinePrintsNothingWhenDaemonUnreachable: the session IS attached
// to spillway (HTTPS_PROXY matches the configured proxy address), but the
// admin listener it would ask about staleCA is down. This must degrade the
// same as every other signal already does — silently, not with a stale or
// half-rendered line.
func TestStatuslinePrintsNothingWhenDaemonUnreachable(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "spillway.yaml")
	// admin.addr deliberately names nothing listening; proxy.port is the
	// one attachedToSpillway checks HTTPS_PROXY against.
	if err := os.WriteFile(cfgPath, []byte(
		"admin:\n  addr: 127.0.0.1:1\nproxy:\n  host: 127.0.0.1\n  port: 61987\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPILLWAY_CONFIG", cfgPath)
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:61987")
	t.Setenv("https_proxy", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")

	got := captureStdout(t, func() {
		if err := runStatusline(nil); err != nil {
			t.Errorf("runStatusline: %v", err)
		}
	})
	if got != "" {
		t.Errorf("unreachable daemon printed %q, want nothing", got)
	}
}
