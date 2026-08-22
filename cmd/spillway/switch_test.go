package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// "auto" is the word a person types. --auto is the flag; off and none were
// already accepted; bare auto fell through to the default branch and became
// an account name, so it failed with `no account named "auto"` — a confusing
// way to be told about a flag.
func TestParseSwitchArgs(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want switchArgs
		err  bool
	}{
		// "auto" is the word a person types; --auto is the flag. Bare auto
		// used to fall through to the default branch and become an account
		// name, failing with `no account named "auto"`.
		{[]string{"auto"}, switchArgs{auto: true}, false},
		{[]string{"--auto"}, switchArgs{auto: true}, false},
		{[]string{"off"}, switchArgs{auto: true}, false},
		{[]string{"none"}, switchArgs{auto: true}, false},
		{[]string{"work"}, switchArgs{name: "work"}, false},
		{[]string{"work", "--force"}, switchArgs{name: "work", force: true}, false},
		{[]string{"-f", "work"}, switchArgs{name: "work", force: true}, false},
		{nil, switchArgs{}, true},
		{[]string{"--nope"}, switchArgs{}, true},
	} {
		got, err := parseSwitchArgs(tc.args)
		if (err != nil) != tc.err {
			t.Errorf("%v: err = %v, want error = %v", tc.args, err, tc.err)
			continue
		}
		if err == nil && got != tc.want {
			t.Errorf("%v: got %+v, want %+v", tc.args, got, tc.want)
		}
	}
}

// A human typing `spillway switch arena` must not see `no account named
// "arena"` — the daemon matches `name` exactly, so runSwitch has to resolve
// a label to the real name before it ever calls POST /api/pin (#23).
func TestRunSwitchResolvesLabelBeforePinning(t *testing.T) {
	var pinnedAccount string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts":
			io.WriteString(w, `[
				{"name":"ckitch@work.example","label":"work","state":"ok"},
				{"name":"ckitch@arena.example","label":"arena","state":"ok"}
			]`)
		case "/api/pin":
			var body struct {
				Account string `json:"account"`
				Force   bool   `json:"force"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			pinnedAccount = body.Account
			io.WriteString(w, `{"pinned":"`+body.Account+`"}`)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()
	writeCfg(t, strings.TrimPrefix(srv.URL, "http://"))

	out := captureStdout(t, func() {
		if err := runSwitch([]string{"work"}); err != nil {
			t.Fatal(err)
		}
	})
	if pinnedAccount != "ckitch@work.example" {
		t.Errorf("POST /api/pin carried account %q, want the resolved name", pinnedAccount)
	}
	if !strings.Contains(out, "ckitch@work.example") {
		t.Errorf("output does not confirm the resolved account:\n%s", out)
	}
}

// Ambiguous input must never reach the daemon at all — resolving is the
// CLI's job precisely so it can refuse without pinning anything (#23).
func TestRunSwitchAmbiguousNeverCallsPin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts":
			io.WriteString(w, `[
				{"name":"ckitch@work.example","label":"work","state":"ok"},
				{"name":"ckitch@work2.example","label":"work-backup","state":"ok"}
			]`)
		case "/api/pin":
			t.Fatal("POST /api/pin was called for an ambiguous name; resolution must refuse before it ever dials the daemon's pin endpoint")
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()
	writeCfg(t, strings.TrimPrefix(srv.URL, "http://"))

	err := runSwitch([]string{"wor"})
	if err == nil {
		t.Fatal("want an error for an ambiguous name, got nil")
	}
	for _, want := range []string{"ckitch@work.example", "ckitch@work2.example"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %q:\n%v", want, err)
		}
	}
}

// Bare `spillway switch` must report — not the usage error it used to be —
// and must say what is pinned plus what could be switched to instead (#23).
func TestRunSwitchBareReportsPinnedAndCandidates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/state":
			io.WriteString(w, `{"pinned":"ckitch@work.example"}`)
		case "/api/accounts":
			io.WriteString(w, `[
				{"name":"ckitch@work.example","label":"work","state":"ok"},
				{"name":"ckitch@arena.example","label":"arena","state":"ok","paid":true},
				{"name":"ckitch@old.example","label":"old","state":"parked"},
				{"name":"ckitch@dead.example","label":"dead","state":"disabled"}
			]`)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()
	writeCfg(t, strings.TrimPrefix(srv.URL, "http://"))

	out := captureStdout(t, func() {
		if err := runSwitch(nil); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "pinned to") || !strings.Contains(out, "work") {
		t.Errorf("does not report the pinned account:\n%s", out)
	}
	if !strings.Contains(out, "arena") || !strings.Contains(out, "would spend money") {
		t.Errorf("does not mark the paid account:\n%s", out)
	}
	if !strings.Contains(out, "old") || !strings.Contains(out, "parked") {
		t.Errorf("does not mark the parked account:\n%s", out)
	}
	if !strings.Contains(out, "dead") || !strings.Contains(out, "disabled") {
		t.Errorf("does not mark the disabled account:\n%s", out)
	}
	// Not spillway status: no headroom bars, no windows, no traffic.
	for _, absent := range []string{"headroom", "quotaWindows", "in flight"} {
		if strings.Contains(out, absent) {
			t.Errorf("bare report leaked status-style detail %q:\n%s", absent, out)
		}
	}
}

// With nothing pinned, the report must say selection is automatic rather
// than naming an account nobody chose.
func TestRunSwitchBareReportsAutomaticSelection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/state":
			io.WriteString(w, `{}`)
		case "/api/accounts":
			io.WriteString(w, `[{"name":"ckitch@work.example","label":"work","state":"ok"}]`)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()
	writeCfg(t, strings.TrimPrefix(srv.URL, "http://"))

	out := captureStdout(t, func() {
		if err := runSwitch(nil); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "automatic") {
		t.Errorf("does not report automatic selection:\n%s", out)
	}
	if strings.Contains(out, "pinned to") {
		t.Errorf("claims a pin while nothing is pinned:\n%s", out)
	}
}

// switchNote is what tells a reader a switch would be refused or would cost
// money before they try it — pinned beats every other note, since it is
// simply true, and the refusal states take priority over "would cost".
func TestSwitchNote(t *testing.T) {
	for _, tc := range []struct {
		name   string
		a      switchAccount
		pinned string
		want   string
	}{
		{"pinned", switchAccount{Name: "a", State: "ok"}, "a", "pinned"},
		{"parked", switchAccount{Name: "a", State: "parked"}, "", "parked"},
		{"disabled", switchAccount{Name: "a", State: "disabled"}, "", "disabled"},
		{"paid", switchAccount{Name: "a", State: "ok", Paid: true}, "", "would spend money (paid extra usage)"},
		{"over threshold", switchAccount{Name: "a", State: "ok", OverThreshold: true}, "", "spent"},
		{"plain", switchAccount{Name: "a", State: "ok"}, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := switchNote(tc.a, tc.pinned); got != tc.want {
				t.Errorf("switchNote(%+v, %q) = %q, want %q", tc.a, tc.pinned, got, tc.want)
			}
		})
	}
}

// The hint has to print after the error, because main prints the error and a
// hint written to stderr first landed above the thing it advises on.
func TestHintedErrorPutsAdviceAfterTheFailure(t *testing.T) {
	e := &hintedError{err: errors.New("pinning there would spend money (409)"), hint: "re-run with --force if you mean it"}
	msg := e.Error()
	if strings.Index(msg, "spend money") > strings.Index(msg, "--force") {
		t.Errorf("advice precedes the failure:\n%s", msg)
	}
	if !errors.Is(e, e.err) {
		t.Error("wrapped error is not unwrappable, so errors.Is on the cause fails")
	}
}
