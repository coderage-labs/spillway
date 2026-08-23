package accounts

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

// get fetches a callback URL. It returns an error rather than failing the
// test, because every caller here runs it on another goroutine to simulate
// the browser — and t.Fatalf off the test goroutine is a vet error and a
// silently ignored failure.
func get(url string) (int, string, error) {
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Get(url)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	buf := make([]byte, 2048)
	n, _ := resp.Body.Read(buf)
	return resp.StatusCode, string(buf[:n]), nil
}

func TestCallbackDeliversTheCode(t *testing.T) {
	cs, err := StartCallback("st-123")
	if err != nil {
		t.Skipf("port %d unavailable: %v", LoopbackPort, err)
	}
	defer cs.Close()

	go func() { _, _, _ = get(cs.RedirectURI() + "?code=abc123&state=st-123") }()

	code, err := cs.Wait(context.Background(), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if code != "abc123" {
		t.Errorf("code = %q, want abc123", code)
	}
}

// Without a state check, any page the user happens to visit while the server
// is up could drive a code of its own into the login.
func TestCallbackRejectsAForeignState(t *testing.T) {
	cs, err := StartCallback("st-123")
	if err != nil {
		t.Skipf("port %d unavailable: %v", LoopbackPort, err)
	}
	defer cs.Close()

	go func() { _, _, _ = get(cs.RedirectURI() + "?code=attacker&state=wrong") }()

	code, err := cs.Wait(context.Background(), 3*time.Second)
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
	cs, err := StartCallback("st-123")
	if err != nil {
		t.Skipf("port %d unavailable: %v", LoopbackPort, err)
	}
	defer cs.Close()

	go func() {
		_, _, _ = get(cs.RedirectURI() + "?error=access_denied&error_description=User+refused&state=st-123")
	}()

	if _, err := cs.Wait(context.Background(), 3*time.Second); err == nil ||
		!strings.Contains(err.Error(), "access_denied") {
		t.Errorf("error = %v, want the provider's access_denied", err)
	}
}

// A browser keeps history, and the page is rendered from URL parameters.
func TestCallbackPageLeaksNeitherCodeNorState(t *testing.T) {
	cs, err := StartCallback("st-secret")
	if err != nil {
		t.Skipf("port %d unavailable: %v", LoopbackPort, err)
	}
	defer cs.Close()

	status, body, err := get(cs.RedirectURI() + "?code=tok-secret&state=st-secret")
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d", status)
	}
	for _, secret := range []string{"tok-secret", "st-secret"} {
		if strings.Contains(body, secret) {
			t.Errorf("callback page echoes %q back into browser history:\n%s", secret, body)
		}
	}
	<-time.After(50 * time.Millisecond)
}

// The exchange repeats the redirect_uri and the provider compares it, so the
// two must be the same string.
func TestRedirectURIMatchesTheAuthorizeRequest(t *testing.T) {
	cs, err := StartCallback("st")
	if err != nil {
		t.Skipf("port %d unavailable: %v", LoopbackPort, err)
	}
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
	first, err := StartCallback("a")
	if err != nil {
		t.Skipf("port %d unavailable: %v", LoopbackPort, err)
	}
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

	first, err := StartCallback("a")
	if err != nil {
		t.Skipf("port %d unavailable: %v", LoopbackPort, err)
	}
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

	first, err := StartCallback("a")
	if err != nil {
		t.Skipf("port %d unavailable: %v", LoopbackPort, err)
	}
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
	var third *CallbackServer
	deadline := time.Now().Add(3 * time.Second)
	for {
		var err error
		third, err = StartCallback("c")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("127.0.0.1 never became bindable after the failed bind rolled back, "+
				"so it was leaked rather than slow to release: %v", err)
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

	first, err := StartCallback("a")
	if err != nil {
		t.Skipf("port %d unavailable: %v", LoopbackPort, err)
	}
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
func TestStartCallback_SingleFamilyHostStillWorks(t *testing.T) {
	withFamilies(t, []string{"127.0.0.1"})

	cs, err := StartCallback("st-single")
	if err != nil {
		t.Skipf("port %d unavailable: %v", LoopbackPort, err)
	}
	defer cs.Close()

	go func() { _, _, _ = get(cs.RedirectURI() + "?code=abc123&state=st-single") }()

	code, err := cs.Wait(context.Background(), 3*time.Second)
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

	first, err := StartCallback("a")
	if err != nil {
		t.Skipf("port %d unavailable: %v", LoopbackPort, err)
	}
	defer first.Close()

	_, err = StartCallback("b")
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
