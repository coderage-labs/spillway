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
