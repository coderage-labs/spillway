package main

// Issue #192: `spillway probe <account> [--force]`.
//
// Everything here runs against an httptest server on an ephemeral port,
// pointed at by a config in t.TempDir() — never the real daemon, never
// 7654/7657, never the user's config.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseProbeArgs(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want probeArgs
		err  bool
	}{
		{[]string{"work"}, probeArgs{name: "work"}, false},
		{[]string{"work", "--force"}, probeArgs{name: "work", force: true}, false},
		{[]string{"-f", "work"}, probeArgs{name: "work", force: true}, false},
		// No account is a usage error, not a pool-wide probe: forcing every
		// account at once is a different feature with a different money
		// question, and silently guessing which was meant is worse than
		// asking.
		{nil, probeArgs{}, true},
		{[]string{"--force"}, probeArgs{force: true}, true},
		{[]string{"--nope", "work"}, probeArgs{}, true},
		// Two names is a typo — probably a shell glob — and probing the last
		// one silently is the kind of guess that spends money.
		{[]string{"work", "spare"}, probeArgs{name: "work"}, true},
	} {
		got, err := parseProbeArgs(tc.args)
		if (err != nil) != tc.err {
			t.Errorf("%v: err = %v, want error = %v", tc.args, err, tc.err)
			continue
		}
		if err == nil && got != tc.want {
			t.Errorf("%v: got %+v, want %+v", tc.args, got, tc.want)
		}
	}
}

// The daemon matches the account name exactly, so a label has to be resolved
// at the edge — the same rule `spillway switch` learned in #23. Probing the
// wrong account is worse than failing: on an opted-in account it spends.
func TestRunProbeResolvesLabelBeforeProbing(t *testing.T) {
	var got struct {
		Name  string `json:"name"`
		Force bool   `json:"force"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts":
			io.WriteString(w, `[
				{"name":"ckitch@work.example","label":"work","state":"ok"},
				{"name":"ckitch@arena.example","label":"arena","state":"ok"}
			]`)
		case "/api/accounts/probe":
			json.NewDecoder(r.Body).Decode(&got)
			io.WriteString(w, `{"account":"`+got.Name+`","quotaWindows":[{"name":"7d","limit":1,"used":0}]}`)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()
	writeCfg(t, strings.TrimPrefix(srv.URL, "http://"))

	out := captureStdout(t, func() {
		if err := runProbe([]string{"arena"}); err != nil {
			t.Fatal(err)
		}
	})
	if got.Name != "ckitch@arena.example" {
		t.Errorf("POST carried name %q, want the resolved account name", got.Name)
	}
	if got.Force {
		t.Error("force was sent without being asked for: the money escape must be explicit")
	}
	// The reading is the answer to the question, so it has to be printed.
	if !strings.Contains(out, "ckitch@arena.example") || !strings.Contains(out, "7d") {
		t.Errorf("output does not report the account and its windows:\n%s", out)
	}
	if strings.Contains(strings.ToLower(out), "charged") {
		t.Errorf("a free probe claimed it was charged:\n%s", out)
	}
}

// An ambiguous name must never reach the daemon: resolving is the CLI's job
// precisely so it can refuse before anything is sent.
func TestRunProbeAmbiguousNameNeverProbes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts":
			io.WriteString(w, `[
				{"name":"ckitch@work.example","label":"work","state":"ok"},
				{"name":"ckitch@work2.example","label":"work-backup","state":"ok"}
			]`)
		case "/api/accounts/probe":
			t.Fatal("POST /api/accounts/probe was called for an ambiguous name")
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()
	writeCfg(t, strings.TrimPrefix(srv.URL, "http://"))

	err := runProbe([]string{"wor"})
	if err == nil {
		t.Fatal("want an error for an ambiguous name, got nil")
	}
	for _, want := range []string{"ckitch@work.example", "ckitch@work2.example"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %q:\n%v", want, err)
		}
	}
}

// The money path, end to end through the CLI: a 409 is relayed with the
// daemon's own reason plus the one next step that answers it, and --force is
// never sent on the user's behalf.
func TestRunProbeRelaysTheChargeRefusalAndOffersForce(t *testing.T) {
	var forces []bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts":
			io.WriteString(w, `[{"name":"paid","label":"paid","state":"exhausted","paid":true}]`)
		case "/api/accounts/probe":
			var body struct {
				Force bool `json:"force"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			forces = append(forces, body.Force)
			if !body.Force {
				w.WriteHeader(http.StatusConflict)
				io.WriteString(w, `probing there would spend money: "paid" is out of quota and has extra usage permitted, so this probe is a charged request`)
				return
			}
			io.WriteString(w, `{"account":"paid","billed":true,"quotaWindows":[{"name":"7d","limit":1,"used":1}]}`)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()
	writeCfg(t, strings.TrimPrefix(srv.URL, "http://"))

	err := runProbe([]string{"paid"})
	if err == nil {
		t.Fatal("an unforced probe of a billable account succeeded; it must be refused")
	}
	if !strings.Contains(err.Error(), "spend money") {
		t.Errorf("error drops the daemon's reason:\n%v", err)
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("error does not offer the one step that answers it:\n%v", err)
	}
	if len(forces) != 1 || forces[0] {
		t.Fatalf("force flags sent = %v, want exactly one unforced attempt", forces)
	}

	// Forced, it goes through — and says out loud that it cost something.
	out := captureStdout(t, func() {
		if err := runProbe([]string{"paid", "--force"}); err != nil {
			t.Fatal(err)
		}
	})
	if len(forces) != 2 || !forces[1] {
		t.Fatalf("force flags sent = %v, want the second attempt forced", forces)
	}
	if !strings.Contains(strings.ToLower(out), "charged") {
		t.Errorf("a charged probe did not say so:\n%s", out)
	}
}

// A 409 that comes back despite --force must not be dressed up as advice to
// force again — the user already did.
func TestRunProbeDoesNotRepeatTheForceHintWhenAlreadyForced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts":
			io.WriteString(w, `[{"name":"paid","label":"paid","state":"exhausted"}]`)
		case "/api/accounts/probe":
			w.WriteHeader(http.StatusConflict)
			io.WriteString(w, `probing there would spend money: "paid"`)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()
	writeCfg(t, strings.TrimPrefix(srv.URL, "http://"))

	err := runProbe([]string{"paid", "--force"})
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	if strings.Contains(err.Error(), "--force") {
		t.Errorf("told the user to force a probe they already forced:\n%v", err)
	}
}

// A window with no limit, or one nothing has re-measured since its own reset
// (#135), has no percentage to print. Rendering either as a bare number is
// the bug #135 fixed, reintroduced on a new surface.
func TestDescribeProbeWindowDoesNotInventAReading(t *testing.T) {
	for _, tc := range []struct {
		w    probeWindow
		want string
	}{
		{probeWindow{Name: "5h", Limit: 1, Used: 0.42}, "42%"},
		{probeWindow{Name: "7d", Limit: 1, Used: 0}, "0%"},
		{probeWindow{Name: "7d", Limit: 0, Used: 0}, "no limit reported"},
		{probeWindow{Name: "7d", Limit: 1, Used: 1, Expired: true}, "expired (no reading since its reset)"},
	} {
		if got := describeProbeWindow(tc.w); got != tc.want {
			t.Errorf("describeProbeWindow(%+v) = %q, want %q", tc.w, got, tc.want)
		}
	}
}
