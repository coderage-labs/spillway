package admin

// Issue #176: /api/state must surface how much traffic spillway passed
// through without a pooled credential because it did not recognise the
// path.
//
// This is the "surface it somewhere durable" half of the inversion's
// mitigation. The log warning fires once per new endpoint shape and then
// scrolls away; this is the figure that is still there when someone goes
// looking, which is the thing that was missing when the two bugs that
// motivated the inversion ran for thousands of requests unnoticed.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestStateReportsUnpooled: the wired source's three figures reach the JSON.
func TestStateReportsUnpooled(t *testing.T) {
	srv, _ := newTestServer(t)
	front := httptest.NewServer(srv)
	defer front.Close()
	srv.SetUnpooled(func() (int, int, []string) {
		return 7500, 11, []string{"/v1/responses"}
	})

	req, err := authed(front.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st stateJSON
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.Unpooled == nil {
		t.Fatal("no unpooled section — the unrecognised-path count must be visible without reading the log")
	}
	if st.Unpooled.Requests != 7500 {
		t.Errorf("requests = %d, want 7500", st.Unpooled.Requests)
	}
	if st.Unpooled.Paths != 11 {
		t.Errorf("paths = %d, want 11", st.Unpooled.Paths)
	}
	if len(st.Unpooled.InferenceShaped) != 1 || st.Unpooled.InferenceShaped[0] != "/v1/responses" {
		t.Errorf("inferenceShaped = %v, want [/v1/responses]", st.Unpooled.InferenceShaped)
	}
}

// TestStateOmitsUnpooledWhenThereIsNone: a healthy daemon that has passed
// nothing through unpooled reports no section at all, rather than a zero
// that would read as a live signal. Holding above works the same way.
func TestStateOmitsUnpooledWhenThereIsNone(t *testing.T) {
	srv, _ := newTestServer(t)
	front := httptest.NewServer(srv)
	defer front.Close()
	srv.SetUnpooled(func() (int, int, []string) { return 0, 0, nil })

	req, err := authed(front.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["unpooled"]; ok {
		t.Errorf("unpooled section present with nothing to report: %v", raw["unpooled"])
	}
}

// TestStateWithoutAnUnpooledSourceIsSilent: a build that never wires a
// proxy handler up (tests, a read-only API harness) must report nothing
// rather than a misleading zero.
func TestStateWithoutAnUnpooledSourceIsSilent(t *testing.T) {
	srv, _ := newTestServer(t)
	front := httptest.NewServer(srv)
	defer front.Close()

	req, err := authed(front.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["unpooled"]; ok {
		t.Error("unpooled section present with no source wired")
	}
}

// TestStateReadsTheUnpooledSourceFresh: the count must not be cached. Its
// whole value is that it is current — a reader checking whether spillway is
// missing something needs now, not startup.
func TestStateReadsTheUnpooledSourceFresh(t *testing.T) {
	srv, _ := newTestServer(t)
	front := httptest.NewServer(srv)
	defer front.Close()
	n := 1
	srv.SetUnpooled(func() (int, int, []string) { return n, 1, nil })

	get := func() int {
		t.Helper()
		req, err := authed(front.URL + "/api/state")
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var st stateJSON
		if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
			t.Fatal(err)
		}
		if st.Unpooled == nil {
			t.Fatal("no unpooled section")
		}
		return st.Unpooled.Requests
	}

	if got := get(); got != 1 {
		t.Fatalf("first read = %d, want 1", got)
	}
	n = 42
	if got := get(); got != 42 {
		t.Errorf("second read = %d, want 42 — /api/state cached a stale count", got)
	}
}
