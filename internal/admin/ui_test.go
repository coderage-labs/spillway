package admin

import (
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
)

// TestDashboardJS runs the embedded dashboard's JavaScript against a fake DOM
// (testdata/ui_dom_test.js) and asserts it renders accounts, quota bars and
// the request table, and that the 5s poll tick re-fetches BOTH endpoints.
//
// The poll is what stops the dashboard freezing during normal traffic:
// successful requests deliberately emit no SSE events, so an SSE-only refresh
// leaves quota bars and the request log static while the pool is being used.
//
// Node is a test-only convenience, never a build dependency — the repo stays
// `go build`-only, so this skips where node is unavailable (e.g. CI images
// without it).
func TestDashboardJS(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping dashboard JS smoke test")
	}
	out, err := exec.Command(node, "testdata/ui_dom_test.js", "static/index.html").CombinedOutput()
	t.Logf("dashboard JS harness output:\n%s", out)
	if err != nil {
		t.Fatalf("dashboard JS smoke test failed: %v", err)
	}
	if strings.Contains(string(out), "FAIL:") {
		t.Fatalf("dashboard JS assertions failed")
	}
}

// The logo is the favicon and the header mark, so a malformed file is a
// broken tab icon on every page load — and SVG failures are silent.
func TestLogoIsWellFormedAndSelfContained(t *testing.T) {
	b, err := staticFS.ReadFile("static/logo.svg")
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := xml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("logo.svg is not valid XML: %v", err)
	}
	svg := string(b)
	// Served from a loopback binary with no network guarantees, and used as a
	// favicon, so it must not reach for anything.
	for _, forbidden := range []string{"<image", "xlink:href", "<script", "@import", "url(http"} {
		if strings.Contains(svg, forbidden) {
			t.Errorf("logo.svg reaches outside itself: %q", forbidden)
		}
	}
	// A viewBox is what lets it render at 22px in the header and 16px in a
	// tab; fixed width/height alone does not scale.
	if !strings.Contains(svg, "viewBox=") {
		t.Error("logo.svg has no viewBox, so it will not scale")
	}
	if !strings.Contains(svg, "aria-label") {
		t.Error("logo.svg has no accessible name")
	}
}

// Serving it matters as much as having it: the dashboard references /logo.svg
// by URL, and only index.html was routed before.
func TestLogoIsServed(t *testing.T) {
	s, _ := newTestServer(t)
	front := httptest.NewServer(s)
	defer front.Close()

	req, err := authed(front.URL + "/logo.svg")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /logo.svg = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/svg+xml" {
		t.Errorf("content-type = %q; a browser will not render it as an icon", ct)
	}
}

// The page must actually reference it, or the file is dead weight.
func TestDashboardUsesTheLogo(t *testing.T) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(b)
	if !strings.Contains(page, `rel="icon"`) || !strings.Contains(page, "/logo.svg") {
		t.Error("dashboard does not use logo.svg as its favicon")
	}
}
