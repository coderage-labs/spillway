package accounts

import (
	"context"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// backstop is the only wall clock left in these tests, and nothing on a
// healthy run waits on it.
//
// deliver returns only once the browser has read the whole response, and
// finish hands the result to a buffered channel before the handler returns —
// so by the time any test calls Wait, the value it wants is already sitting
// in that channel and Wait returns without touching its timer. The backstop
// therefore fires only for a server that accepted the connection and then
// never answered, which is a hang and not a slow runner. Being generous
// costs a genuinely hung run some seconds and costs a healthy run nothing:
// the opposite trade to the 3s budgets it replaces, which every test had to
// out-run on every run.
const backstop = 30 * time.Second

// browserResult is what the simulated browser saw.
//
// The error is carried rather than discarded because it is the whole
// diagnosis. Before #98 these tests threw the client's error away and let a
// wall-clock deadline expire instead, so "the browser was refused instantly"
// and "the runner is slow" both surfaced as the same bare "timed out after
// 3s waiting for the browser" — a message that names neither the address
// dialled nor what went wrong.
type browserResult struct {
	url    string
	status int
	body   string
	err    error
	took   time.Duration
}

// callbackURLs builds one URL per address the server actually bound.
//
// Deliberately not RedirectURI(): that carries the "localhost" name the
// provider has registered, and resolving it is the resolver's business, not
// this package's. macOS ships "::1 localhost" in /etc/hosts and Go sorts ::1
// ahead of 127.0.0.1, so "localhost" is dialled on IPv6 first; Debian and
// Ubuntu map ::1 to ip6-localhost instead, leaving "localhost" as 127.0.0.1
// alone. That asymmetry is the whole of #98. A test that pins the server to
// one family and then dials the name is, on macOS only, relying on Go's
// Happy Eyeballs fallback out of the family it deliberately excluded — a
// 300ms timer plus scheduler latency, measured here at ~70µs idle but a
// ~200ms median and >3s under contention. Dialling the address the server
// bound tests the server rather than the host's resolver.
func callbackURLs(cs *CallbackServer, query string) []string {
	urls := make([]string, 0, len(cs.lns))
	for _, ln := range cs.lns {
		urls = append(urls, "http://"+ln.Addr().String()+"/callback"+query)
	}
	return urls
}

// fetch performs one simulated browser request and reports everything it saw.
// It never fails the test itself, so it is safe to call from a goroutine.
// The return value is named so the deferred timing lands on the value the
// caller receives; assigning to a local after `return res` has copied it
// reports every request as having taken 0s, which is worse than not timing
// it at all.
func fetch(ctx context.Context, url string) (res browserResult) {
	res = browserResult{url: url}
	start := time.Now()
	defer func() { res.took = time.Since(start) }()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		res.err = err
		return res
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		res.err = err
		return res
	}
	defer resp.Body.Close()
	// The whole body, not the first read of it. A single Read returns
	// whatever one chunk happens to hold, so a test scanning the page for a
	// leaked code or state could miss one that landed past the split and
	// pass without ever having looked.
	body, err := io.ReadAll(resp.Body)
	res.status, res.body, res.err = resp.StatusCode, string(body), err
	return res
}

// deliver drives one simulated browser callback and returns what the browser
// saw, failing the test with the browser's own error if it could not deliver.
//
// Synchronous, and that is the fix. The callback used to be fired on a
// goroutine whose error was discarded, leaving the test to sit out a 3s
// deadline that raced the client's own 3s deadline — two magic numbers of
// equal size where the loser carried the reason. No goroutine is needed:
// finish writes the page and hands the result to a channel buffered for one,
// so it never blocks on a reader, and the response cannot complete until the
// handler has returned. A finished response therefore *proves* the result is
// already waiting. This synchronises on the event instead of estimating how
// long it takes.
func deliver(t *testing.T, cs *CallbackServer, query string) browserResult {
	t.Helper()

	urls := callbackURLs(cs, query)
	if len(urls) == 0 {
		t.Fatal("callback server bound no listeners")
	}
	ctx, cancel := context.WithTimeout(context.Background(), backstop)
	defer cancel()

	res := fetch(ctx, urls[0])
	if res.err != nil {
		t.Fatalf("the simulated browser never reached the callback server: "+
			"GET %s failed after %s: %v\n"+
			"(the server reported binding %v)", res.url, res.took, res.err, boundAddrs(cs))
	}
	return res
}

// awaitCode collects the result that the delivered callback has already
// handed over.
//
// Nothing here should ever wait: deliver returned only once the response was
// complete, and finish puts the result in a buffered channel before the
// handler returns, so the value is sitting there. The backstop therefore
// covers one specific fault — the browser was served and the result was
// dropped on the way to Wait — and that deserves its own message rather than
// Wait's "timed out waiting for the browser", which would blame a browser
// that demonstrably did its job.
//
// The context is given the shorter budget so a backstop hit comes back as
// context.DeadlineExceeded, distinguishable from a real error the handler
// put on the channel (a state mismatch, a provider refusal), which is passed
// through untouched.
func awaitCode(t *testing.T, cs *CallbackServer, res browserResult) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), backstop)
	defer cancel()

	code, err := cs.Wait(ctx, 2*backstop)
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the browser was served but no result reached Wait within %s: "+
			"GET %s returned %d in %s, so the callback ran and the result was "+
			"lost on the way out", backstop, res.url, res.status, res.took)
	}
	return code, err
}

// startOrSkip starts a callback server, skipping only when the port is
// genuinely held by something else on this machine.
//
// The blanket "skip on any error" this replaces was a hole big enough to
// hide the very regressions these tests exist for: make StartCallback refuse
// a single-family host and TestStartCallback_SingleFamilyHostStillWorks
// would have reported a skip and gone green. The two errors are already
// written to be told apart — see
// TestStartCallback_NoFamilyErrorDistinctFromPortInUse — and this is the
// caller finally acting on that distinction instead of treating every
// failure as somebody else's port.
func startOrSkip(t *testing.T, state string) *CallbackServer {
	t.Helper()
	cs, err := StartCallback(state)
	if err == nil {
		return cs
	}
	if strings.Contains(err.Error(), "in use") {
		t.Skipf("port %d is held by something else on this machine: %v", LoopbackPort, err)
	}
	t.Fatalf("StartCallback failed for a reason other than the port being taken: %v", err)
	return nil
}

// boundAddrs is for failure messages: "nothing arrived" is only actionable
// next to where the server was actually listening.
func boundAddrs(cs *CallbackServer) []string {
	var addrs []string
	for _, ln := range cs.lns {
		addrs = append(addrs, ln.Addr().String())
	}
	return addrs
}

func TestCallbackDeliversTheCode(t *testing.T) {
	cs := startOrSkip(t, "st-123")
	defer cs.Close()

	code, err := awaitCode(t, cs, deliver(t, cs, "?code=abc123&state=st-123"))
	if err != nil {
		t.Fatal(err)
	}
	if code != "abc123" {
		t.Errorf("code = %q, want abc123", code)
	}
}

// Every family the server bound must actually answer /callback. "localhost"
// can resolve to any of them and the browser picks; a family that binds but
// does not serve is a login that hangs on some machines and not others,
// which is precisely the class of bug #51 was about.
//
// Concurrent on purpose. Both families share one handler and one result
// channel, so this is also the test that a browser trying both at once — or
// prefetching, or a restored tab — cannot wedge the server. With the
// once-only check in finish left unguarded, both goroutines can see seen as
// false and both send; the channel holds one, so the second send blocks
// forever and takes Close's Shutdown down with it.
func TestEveryBoundFamilyServesTheCallback(t *testing.T) {
	cs := startOrSkip(t, "st-fam")
	defer cs.Close()

	ctx, cancel := context.WithTimeout(context.Background(), backstop)
	defer cancel()

	urls := callbackURLs(cs, "?code=abc123&state=st-fam")
	results := make([]browserResult, len(urls))
	var wg sync.WaitGroup
	for i, u := range urls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = fetch(ctx, u)
		}()
	}
	wg.Wait()

	for _, res := range results {
		if res.err != nil {
			t.Errorf("GET %s failed after %s: %v", res.url, res.took, res.err)
			continue
		}
		if res.status != http.StatusOK {
			t.Errorf("GET %s: status = %d, want 200", res.url, res.status)
		}
	}

	// Exactly one delivery, however many families answered.
	code, err := awaitCode(t, cs, results[0])
	if err != nil {
		t.Fatalf("no code arrived from any of %v: %v", boundAddrs(cs), err)
	}
	if code != "abc123" {
		t.Errorf("code = %q, want abc123", code)
	}
}

// Without a state check, any page the user happens to visit while the server
// is up could drive a code of its own into the login.
func TestCallbackRejectsAForeignState(t *testing.T) {
	cs := startOrSkip(t, "st-123")
	defer cs.Close()

	code, err := awaitCode(t, cs, deliver(t, cs, "?code=attacker&state=wrong"))
	if err == nil {
		t.Fatalf("accepted a callback with the wrong state, code %q", code)
	}
	if !strings.Contains(err.Error(), "state mismatch") {
		t.Errorf("error = %v, want a state mismatch", err)
	}
}

// The provider reports a declined consent through the redirect; surfacing it
// beats a five-minute timeout with no explanation.
func TestCallbackSurfacesProviderErrors(t *testing.T) {
	cs := startOrSkip(t, "st-123")
	defer cs.Close()

	res := deliver(t, cs, "?error=access_denied&error_description=User+refused&state=st-123")

	if _, err := awaitCode(t, cs, res); err == nil ||
		!strings.Contains(err.Error(), "access_denied") {
		t.Errorf("error = %v, want the provider's access_denied", err)
	}
}

// A browser keeps history, and the page is rendered from URL parameters.
func TestCallbackPageLeaksNeitherCodeNorState(t *testing.T) {
	cs := startOrSkip(t, "st-secret")
	defer cs.Close()

	res := deliver(t, cs, "?code=tok-secret&state=st-secret")
	if res.status != http.StatusOK {
		t.Errorf("status = %d", res.status)
	}
	for _, secret := range []string{"tok-secret", "st-secret"} {
		if strings.Contains(res.body, secret) {
			t.Errorf("callback page echoes %q back into browser history:\n%s", secret, res.body)
		}
	}
	// No settling sleep. deliver has read the response to EOF, which the
	// handler cannot reach until after it has handed the result over, so
	// there is nothing left in flight for Close to interrupt.
}

// The exchange repeats the redirect_uri and the provider compares it, so the
// two must be the same string.
func TestRedirectURIMatchesTheAuthorizeRequest(t *testing.T) {
	cs := startOrSkip(t, "st")
	defer cs.Close()

	pkce, err := GeneratePKCE()
	if err != nil {
		t.Fatal(err)
	}
	u := AuthorizeURL(cs.RedirectURI(), pkce)
	if !strings.Contains(u, "redirect_uri="+urlEscaped(cs.RedirectURI())) {
		t.Errorf("authorize URL does not carry the callback's own redirect_uri:\n%s", u)
	}
	if cs.RedirectURI() != LoopbackRedirectURI {
		t.Errorf("redirect = %q, want the one confirmed registered", cs.RedirectURI())
	}
}

// A second login while one is running must not silently steal the port.
func TestSecondCallbackServerFailsToBind(t *testing.T) {
	first := startOrSkip(t, "a")
	defer first.Close()

	if second, err := StartCallback("b"); err == nil {
		second.Close()
		t.Error("a second callback server bound the same port — one login would " +
			"receive the other's code")
	}
}

// withFamilies forces loopbackFamilies for the duration of one test, so a
// test can exercise a single-family or dual-family host regardless of what
// this machine actually supports.
func withFamilies(t *testing.T, families []string) {
	t.Helper()
	orig := loopbackFamilies
	loopbackFamilies = func() []string { return families }
	t.Cleanup(func() { loopbackFamilies = orig })
}

// withFamiliesSequence gives successive StartCallback calls different family
// lists — the first call in the sequence gets seqs[0], the second seqs[1],
// and so on (the last entry repeats if more calls are made than entries
// given). This is what actually reproduces the bug from #51: the two
// StartCallback calls in a real partial-bind race don't request the same
// set — one login ends up with fewer families bound than the other asks
// for.
func withFamiliesSequence(t *testing.T, seqs ...[]string) {
	t.Helper()
	orig := loopbackFamilies
	i := 0
	loopbackFamilies = func() []string {
		f := seqs[i]
		if i < len(seqs)-1 {
			i++
		}
		return f
	}
	t.Cleanup(func() { loopbackFamilies = orig })
}

// This is the exact shape of the bug in #51: the first login only has (or
// only requests) one family, and a second login requesting every family the
// host supports must still be refused outright — not allowed to quietly
// take over the family the first login left unbound and end up serving
// /callback on the same port as a different, unrelated login.
func TestSecondLoginCannotStealTheFamilyFirstLoginLeftUnbound(t *testing.T) {
	real := probeLoopbackFamilies()
	if len(real) < 2 {
		t.Skip("this host does not support both loopback families")
	}
	withFamiliesSequence(t, []string{"127.0.0.1"}, real)

	first := startOrSkip(t, "a")
	defer first.Close()

	if second, err := StartCallback("b"); err == nil {
		second.Close()
		t.Fatal("second login bound the family the first login left free — " +
			"one login could receive the other's authorization code")
	}
}

// When a bind partway through the family list fails, any listener already
// opened for an earlier family in this same call must be released — not
// leaked — so the failed attempt does not itself squat on the port.
func TestFailedBindReleasesAlreadyOpenedListener(t *testing.T) {
	real := probeLoopbackFamilies()
	if len(real) < 2 {
		t.Skip("this host does not support both loopback families")
	}
	// First login takes [::1] only. Second login is forced to try
	// 127.0.0.1 (succeeds) then [::1] (fails, already held) — exercising
	// the rollback of the 127.0.0.1 listener it just opened.
	withFamiliesSequence(t, []string{"[::1]"}, []string{"127.0.0.1", "[::1]"}, []string{"127.0.0.1"})

	first := startOrSkip(t, "a")
	defer first.Close()

	if second, err := StartCallback("b"); err == nil {
		second.Close()
		t.Fatal("second login should have failed on the already-held [::1] family")
	}

	// If the failed attempt actually released 127.0.0.1 rather than leaking
	// it, a third call asking only for 127.0.0.1 must succeed.
	//
	// Retried rather than checked once: closing a listener does not make its
	// port instantly rebindable on Windows, and a single immediate attempt
	// read that delay as a leak and failed CI there while passing on macOS
	// and Linux. The retry still distinguishes the two cases — a genuinely
	// leaked listener is held for the life of the process and never becomes
	// bindable, however long this waits.
	//
	// This is the one deadline in the file that must exist: there is no
	// event to wait on, only the kernel eventually releasing the port. It is
	// generous rather than tight for that reason — a healthy run exits on
	// the first or second attempt and never approaches it, so the budget
	// only bounds a genuine leak. The failure says how long it waited and
	// how many attempts it made, so a future reader can tell a slow runner
	// from a listener that is never coming back.
	var third *CallbackServer
	start := time.Now()
	deadline := start.Add(10 * time.Second)
	for attempt := 1; ; attempt++ {
		var err error
		third, err = StartCallback("c")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("127.0.0.1 never became bindable after the failed bind rolled back, "+
				"so it was leaked rather than slow to release: %d attempts over %s, "+
				"last error: %v", attempt, time.Since(start).Round(time.Millisecond), err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	third.Close()
}

// On a host that genuinely supports both loopback families, a second login
// must be refused outright rather than silently taking over whichever
// family the first login left unbound.
func TestSecondCallbackServerFailsToBind_BothFamiliesSupported(t *testing.T) {
	real := probeLoopbackFamilies()
	if len(real) < 2 {
		t.Skip("this host does not support both loopback families")
	}
	withFamilies(t, real)

	first := startOrSkip(t, "a")
	defer first.Close()

	second, err := StartCallback("b")
	if err == nil {
		second.Close()
		t.Fatal("a second callback server bound the same port on a dual-family host")
	}
	if !strings.Contains(err.Error(), "in use") {
		t.Errorf("error = %v, want an 'in use' error", err)
	}
}

// A host that only has one loopback family available (forced here rather
// than relying on the test machine actually lacking one) must still get a
// fully working callback server on that family — requiring "all supported
// families" must not regress into requiring both unconditionally.
//
// The browser here must dial the bound address, not RedirectURI(). Forcing
// one family onto a host that has two builds a state production never has:
// a real single-family host also resolves "localhost" to that one family,
// whereas this machine still resolves it to both. Dialling the name would
// therefore send the callback to ::1 first on macOS — the family this test
// just told the server not to bind — and leave the run depending on the
// Happy Eyeballs fallback to save it. That is #98.
func TestStartCallback_SingleFamilyHostStillWorks(t *testing.T) {
	withFamilies(t, []string{"127.0.0.1"})

	cs := startOrSkip(t, "st-single")
	defer cs.Close()

	code, err := awaitCode(t, cs, deliver(t, cs, "?code=abc123&state=st-single"))
	if err != nil {
		t.Fatal(err)
	}
	if code != "abc123" {
		t.Errorf("code = %q, want abc123", code)
	}
}

// The error for "this host has no usable loopback family" must read
// differently from "the port is already in use" — the first blames the
// host, the second blames a real conflict, and a user or a bug report
// needs to be able to tell them apart.
func TestStartCallback_NoFamilyErrorDistinctFromPortInUse(t *testing.T) {
	withFamilies(t, nil)

	_, err := StartCallback("st-none")
	if err == nil {
		t.Fatal("expected an error when no loopback family is available")
	}
	if strings.Contains(err.Error(), "in use") {
		t.Errorf("no-family error should not read like a port-in-use error: %v", err)
	}
}

// The port genuinely being held by another login (as opposed to this host
// lacking a family) must produce the "in use" error, forced deterministic
// here by pinning both servers to the same single family instead of relying
// on whatever families this machine happens to support.
func TestStartCallback_PortInUseErrorIsDistinguishable(t *testing.T) {
	withFamilies(t, []string{"127.0.0.1"})

	first := startOrSkip(t, "a")
	defer first.Close()

	_, err := StartCallback("b")
	if err == nil {
		t.Fatal("expected the second server to fail to bind")
	}
	if !strings.Contains(err.Error(), "in use") {
		t.Errorf("error = %v, want an 'in use' error", err)
	}
}

func urlEscaped(s string) string {
	return strings.NewReplacer(":", "%3A", "/", "%2F").Replace(s)
}

// The page a browser renders must not depend on anything it has to fetch.
// This listener lives for seconds and then stops; a stylesheet or font from
// elsewhere is latency on the one screen the user is waiting for, and a
// third-party request at the exact moment they finish authenticating.
func TestCallbackPageIsSelfContained(t *testing.T) {
	page := callbackPage(nil)
	// The tank is inline SVG in a data: URI. Strip it before looking for
	// outbound URLs: it contains xmlns='http://www.w3.org/2000/svg', which is
	// a namespace name and not something any browser fetches. A test that
	// flags it is testing the wrong string.
	stripped := regexp.MustCompile(`url\("data:[^"]*"\)`).ReplaceAllString(page, "url(DATA)")
	if !strings.Contains(page, "data:image/svg+xml") {
		t.Error("expected the swell to be an inline data URI")
	}
	for _, forbidden := range []string{
		"http://", "https://", "<script", "<link", "@import", "<img", "srcset",
	} {
		if strings.Contains(stripped, forbidden) {
			t.Errorf("callback page reaches outside itself: found %q", forbidden)
		}
	}
}

// Both outcomes must render, and the failing one must actually say why.
func TestCallbackPageRendersBothOutcomes(t *testing.T) {
	okPage := callbackPage(nil)
	if !strings.Contains(okPage, "Signed in") || strings.Contains(okPage, "class=\"card bad\"") {
		t.Error("success page is not the success page")
	}
	bad := callbackPage(errors.New("authorization failed: access_denied"))
	if !strings.Contains(bad, "Login failed") || !strings.Contains(bad, "access_denied") {
		t.Error("failure page does not state the reason")
	}
	// Dark mode is a media query, not an afterthought — the dashboard has one
	// and a white flash next to it looks broken.
	if !strings.Contains(okPage, "prefers-color-scheme: dark") {
		t.Error("no dark-mode palette")
	}
	if !strings.Contains(okPage, "prefers-reduced-motion") {
		t.Error("the fill animation is not gated on reduced-motion")
	}
}

// Anything from the request would land in browser history via the URL.
func TestCallbackPageEscapesTheErrorText(t *testing.T) {
	page := callbackPage(errors.New(`<script>alert(1)</script>`))
	if strings.Contains(page, "<script>alert(1)</script>") {
		t.Error("error text is injected into the page unescaped")
	}
	if !strings.Contains(page, "&lt;script&gt;") {
		t.Error("expected the error text escaped")
	}
}
