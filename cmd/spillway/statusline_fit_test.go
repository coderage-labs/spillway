package main

// Issue #183, the width half and the privacy half.
//
// A wrapped status line costs a terminal row on EVERY render, so "it fits"
// is a hard requirement rather than a nicety, and what is dropped to make it
// fit is dropped in the order agreed on the issue:
//
//	row 1: tokens/min → git branch → abbreviate the path
//	row 2: 💀 → ✈ → the fable window
//
// And §5: the payload carries a session id, a transcript path and a real
// bill. None of it may reach stdout or a log — only derived numbers leave
// the decoder.

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"testing"
	"time"
)

// demoPool is the pool from the issue: the line that measured 153 columns
// and wrapped in an 80-column terminal. Three quota windows, two requests in
// flight and six spent accounts.
func demoPool(now time.Time) ([]slAccount, slState) {
	a := slAccount{
		Name: "csno@example.com", Label: "csno", State: "ok",
		InFlight: 2, LastModel: "fable-5-1",
		Windows: []slWindow{
			{Name: "5h", Limit: 100, Used: 9, ResetAt: now.Add(2 * time.Hour)},
			{Name: "7d", Limit: 100, Used: 89, ResetAt: now.Add(5*24*time.Hour + time.Hour)},
			{Name: "7d-fable", Limit: 100, Used: 60, ResetAt: now.Add(3 * 24 * time.Hour)},
		},
	}
	return []slAccount{a}, slState{Total: 8, Usable: 2, Exhausted: 6}
}

// demoSession is the session from the issue, with a path long enough that
// row 1 also has to give something up at 80 columns.
func demoSession(t *testing.T) *slSession {
	t.Helper()
	setHome(t, "/Users/kris")
	s := sessionFrom(t, payloadJSON(t, func(m map[string]any) {
		m["workspace"] = map[string]any{"current_dir": "/Users/kris/Repos/coderage/spillway"}
	}))
	if s == nil {
		t.Fatal("the demo payload decoded to nothing")
	}
	s.Branch = "main"
	return s
}

func rowsOf(t *testing.T, line string) (top, pool string) {
	t.Helper()
	r := strings.Split(line, "\n")
	if len(r) != 2 {
		t.Fatalf("expected two rows, got %d: %q", len(r), line)
	}
	return r[0], r[1]
}

// The headline: at 120 columns everything is on, at 80 both rows still fit,
// and neither row ever exceeds the terminal.
func TestRenderedLineFitsTheTerminal(t *testing.T) {
	now := time.Now()
	list, st := demoPool(now)
	sess := demoSession(t)

	for _, cols := range []int{160, 120, 100, 90, 80, 70, 60, 50, 40, 30} {
		line := renderLine(noColour, list, st, sess, now, cols)
		for i, row := range strings.Split(line, "\n") {
			if w := displayWidth(row); w > cols {
				t.Errorf("at %d columns row %d is %d columns wide and will wrap: %q", cols, i, w, row)
			}
		}
		if n := len(strings.Split(line, "\n")); n != 2 {
			t.Errorf("at %d columns the line rendered as %d rows, want 2", cols, n)
		}
	}
}

// The trim order, step by step, at the width where each step is the one that
// makes the difference. Asserting the order and not just the fit: dropping
// the context percentage to keep a tokens-per-minute figure would pass a
// width check and lose the most useful number on the row.
func TestRowOneTrimsInTheAgreedOrder(t *testing.T) {
	now := time.Now()
	list, st := demoPool(now)
	sess := demoSession(t)

	full, _ := rowsOf(t, renderLine(noColour, list, st, sess, now, 0)) // 0: width unknown, no trimming
	for _, want := range []string{"📁 ~/Repos/coderage/spillway", "🌿 main", "🤖 Opus 5 · high", "🧠 34% ctx", "📊 128k (2.1k tpm)"} {
		if !strings.Contains(full, want) {
			t.Errorf("untrimmed row 1 %q is missing %q", full, want)
		}
	}

	// 80: the rate goes first, and nothing else does.
	at80, _ := rowsOf(t, renderLine(noColour, list, st, sess, now, 80))
	if strings.Contains(at80, "tpm") {
		t.Errorf("at 80 columns row 1 %q kept the rate, which is first to go", at80)
	}
	for _, keep := range []string{"📊 128k", "🌿 main", "~/Repos/coderage/spillway", "🧠 34% ctx"} {
		if !strings.Contains(at80, keep) {
			t.Errorf("at 80 columns row 1 %q dropped %q before the rate", at80, keep)
		}
	}

	// 70: the branch goes second, the path is still whole.
	at70, _ := rowsOf(t, renderLine(noColour, list, st, sess, now, 70))
	if strings.Contains(at70, "🌿") {
		t.Errorf("at 70 columns row 1 %q kept the branch", at70)
	}
	if !strings.Contains(at70, "~/Repos/coderage/spillway") {
		t.Errorf("at 70 columns row 1 %q abbreviated the path before dropping the branch", at70)
	}

	// 60: the path is abbreviated third — and still names the directory
	// actually being worked in.
	at60, _ := rowsOf(t, renderLine(noColour, list, st, sess, now, 60))
	if !strings.Contains(at60, "📁 ~/spillway") {
		t.Errorf("at 60 columns row 1 %q did not abbreviate the path to ~/spillway", at60)
	}
	if strings.Contains(at60, "coderage") {
		t.Errorf("at 60 columns row 1 %q still carries the middle of the path", at60)
	}
	// The context percentage survives every documented step: it is the
	// single most behaviour-changing number on the row.
	for _, row := range []string{at80, at70, at60} {
		if !strings.Contains(row, "🧠 34% ctx") {
			t.Errorf("row %q dropped the context window before the lower-value segments", row)
		}
	}
}

func TestRowTwoTrimsInTheAgreedOrder(t *testing.T) {
	now := time.Now()
	list, st := demoPool(now)
	sess := demoSession(t)

	_, full := rowsOf(t, renderLine(noColour, list, st, sess, now, 0))
	for _, want := range []string{"⛁ csno", "🎯 fable-5-1", "⏳", "91% 5h", "📆", "11% 7d", "↻5d1h", "40% 7d-fable", "✈ 2", "💀 6"} {
		if !strings.Contains(full, want) {
			t.Errorf("untrimmed row 2 %q is missing %q", full, want)
		}
	}
	// The whole claim of #183's glyph table: the words are REPLACED, not
	// decorated, which is what makes this a net saving.
	for _, gone := range []string{"in flight", "spent"} {
		if strings.Contains(full, gone) {
			t.Errorf("row 2 %q still spells out %q next to its glyph", full, gone)
		}
	}

	// 97, 92, 66 are where each step becomes necessary. Asserted as a
	// sequence so a reordering shows up as the wrong thing surviving.
	at97, _ := poolRowAt(t, list, st, sess, now, 97)
	if strings.Contains(at97, "💀") {
		t.Errorf("at 97 columns row 2 %q kept the spent count, which is first to go", at97)
	}
	if !strings.Contains(at97, "✈ 2") || !strings.Contains(at97, "7d-fable") {
		t.Errorf("at 97 columns row 2 %q dropped in-flight or the fable window before spent", at97)
	}

	at92, _ := poolRowAt(t, list, st, sess, now, 92)
	if strings.Contains(at92, "✈") {
		t.Errorf("at 92 columns row 2 %q kept the in-flight count", at92)
	}
	if !strings.Contains(at92, "7d-fable") {
		t.Errorf("at 92 columns row 2 %q dropped the fable window before in-flight", at92)
	}

	at66, _ := poolRowAt(t, list, st, sess, now, 66)
	if strings.Contains(at66, "fable-5-1") == false {
		t.Errorf("at 66 columns row 2 %q lost the served model, which is not in the trim order", at66)
	}
	if strings.Contains(at66, "7d-fable") {
		t.Errorf("at 66 columns row 2 %q kept the fable window", at66)
	}
	// The two real quota windows are the row's reason to exist.
	for _, keep := range []string{"91% 5h", "11% 7d"} {
		if !strings.Contains(at66, keep) {
			t.Errorf("at 66 columns row 2 %q dropped %q", at66, keep)
		}
	}
}

// poolRowAt renders at cols and returns the pool row, failing if it did not
// actually fit — a trim step that runs but does not bring the row inside the
// terminal is the wrap this whole mechanism exists to prevent.
func poolRowAt(t *testing.T, list []slAccount, st slState, sess *slSession, now time.Time, cols int) (string, int) {
	t.Helper()
	_, pool := rowsOf(t, renderLine(noColour, list, st, sess, now, cols))
	w := displayWidth(pool)
	if w > cols {
		t.Fatalf("row 2 is %d columns at a %d-column terminal: %q", w, cols, pool)
	}
	return pool, w
}

// Even a terminal narrower than every documented reduction can accommodate
// must not wrap. The last resort is a cut, not an overflow.
func TestAbsurdlyNarrowTerminalStillDoesNotWrap(t *testing.T) {
	now := time.Now()
	list, st := demoPool(now)
	sess := demoSession(t)
	for _, cols := range []int{21, 24, 28} {
		line := renderLine(truecolour, list, st, sess, now, cols)
		for i, row := range strings.Split(line, "\n") {
			if w := displayWidth(row); w > cols {
				t.Errorf("at %d columns row %d is %d wide: %q", cols, i, w, row)
			}
		}
	}
}

// The rendered output at the two widths the PR quotes. Not an assertion
// about the exact text — it is a log, so the numbers in the PR come from the
// code rather than from a hand-written mock-up.
func TestShowRenderedOutput(t *testing.T) {
	now := time.Date(2026, 9, 17, 11, 0, 0, 0, time.UTC)
	list, st := demoPool(now)
	sess := demoSession(t)
	for _, cols := range []int{120, 80} {
		line := renderLine(noColour, list, st, sess, now, cols)
		t.Logf("at %d columns:", cols)
		for _, row := range strings.Split(line, "\n") {
			t.Logf("  %s   [%d cols]", row, displayWidth(row))
		}
	}
}

// ── end to end, through runStatusline ────────────────────────────────────

// The whole point of the issue: a real payload on stdin, a real daemon, and
// a status line that now says something about the session.
func TestStatuslineRendersTheSessionRowFromStdin(t *testing.T) {
	hits, port := statuslineFixture(t, 8, 50)
	setAttachEnv(t, fmt.Sprintf("http://127.0.0.1:%d", port))
	mustBeAttached(t, true)
	setHome(t, "/Users/kris")
	t.Setenv("SPILLWAY_STATUSLINE_COLUMNS", "120")
	withStdin(t, strings.NewReader(payloadJSON(t, nil)))

	got := captureStdout(t, func() {
		if err := runStatusline(nil); err != nil {
			t.Errorf("runStatusline: %v", err)
		}
	})
	top, pool := rowsOf(t, got)
	for _, want := range []string{"📁 ~/Repos/spillway", "🤖 Opus 5", "high", "🧠 34% ctx", "📊 128k"} {
		if !strings.Contains(top, want) {
			t.Errorf("session row %q is missing %q", top, want)
		}
	}
	if !strings.Contains(pool, "⛁ ") || !strings.Contains(pool, "acct-") {
		t.Errorf("pool row %q lost the pool", pool)
	}
	if n := hits.Load(); n < 2 {
		t.Errorf("the daemon served %d admin requests, want both endpoints", n)
	}
}

// Absent, empty, junk and never-closed stdin: the line degrades to the
// pool-only render. It must not fail, must not hang, and must not lose the
// pool — the session row is an addition, not a dependency.
func TestStatuslineDegradesToThePoolOnlyLineWithoutAPayload(t *testing.T) {
	hits, port := statuslineFixture(t, 8, 50)
	setAttachEnv(t, fmt.Sprintf("http://127.0.0.1:%d", port))
	mustBeAttached(t, true)

	for name, in := range map[string]io.Reader{
		"absent":      nil,
		"empty":       strings.NewReader(""),
		"not json":    strings.NewReader("this is not a status line payload\n"),
		"half a json": strings.NewReader(`{"model":{"display_name":"Opus`),
		// A pipe nobody closes promptly — the freeze the original "do not
		// read stdin" rule was written to avoid. A perfectly good payload
		// arrives, far too late, and the line must already have gone
		// without it. The delay is orders of magnitude over stdinBudget so
		// that what is asserted is the degrade, not a stopwatch.
		"a payload that arrives far too late": &slowReader{
			delay: 3 * time.Second, r: strings.NewReader(payloadJSON(t, nil)),
		},
	} {
		t.Run(name, func(t *testing.T) {
			before := hits.Load()
			withStdin(t, in)
			got := captureStdout(t, func() {
				if err := runStatusline(nil); err != nil {
					t.Errorf("runStatusline: %v", err)
				}
			})
			if got == "" {
				t.Fatalf("%s stdin printed nothing at all — the pool line must survive it", name)
			}
			if strings.Contains(got, "\n") {
				t.Errorf("%s stdin produced a session row: %q", name, got)
			}
			if !strings.Contains(got, "acct-") || !strings.Contains(got, "█") {
				t.Errorf("%s stdin produced %q, want the ordinary pool line", name, got)
			}
			if hits.Load() == before {
				t.Errorf("%s stdin: the daemon was never contacted, so this is not the reachable path", name)
			}
		})
	}
}

// slowReader withholds its first byte for delay, then reads normally.
type slowReader struct {
	delay   time.Duration
	r       io.Reader
	started bool
}

func (s *slowReader) Read(p []byte) (int, error) {
	if !s.started {
		s.started = true
		time.Sleep(s.delay)
	}
	return s.r.Read(p)
}

// #165's degraded marker survives #183 exactly as it was, with the session
// row — which knows nothing about whether the daemon answered — above it.
func TestUnreachableDaemonKeepsItsMarkerUnderTheSessionRow(t *testing.T) {
	cfgPath := t.TempDir() + "/spillway.yaml"
	if err := os.WriteFile(cfgPath, []byte(
		"admin:\n  addr: 127.0.0.1:1\nproxy:\n  host: 127.0.0.1\n  port: 61989\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPILLWAY_CONFIG", cfgPath)
	t.Setenv("NO_COLOR", "1")
	setAttachEnv(t, "")
	setHome(t, "/Users/kris")
	withStdin(t, strings.NewReader(payloadJSON(t, nil)))

	got := captureStdout(t, func() {
		if err := runStatusline([]string{"--always"}); err != nil {
			t.Errorf("runStatusline: %v", err)
		}
	})
	top, pool := rowsOf(t, got)
	if pool != "⛁ —" {
		t.Errorf("pool row = %q, want #165's marker byte for byte", pool)
	}
	if !strings.Contains(top, "🤖 Opus 5") {
		t.Errorf("session row %q was lost because the daemon was down — it comes from stdin", top)
	}
}

// ── §5 ───────────────────────────────────────────────────────────────────

// The payload carries a session id, a transcript path and a real bill.
// Nothing identifying may reach stdout, stderr or a log — derived numbers
// only. The needles are what a real payload would carry.
func TestNothingFromThePayloadReachesOutputOrALog(t *testing.T) {
	hits, port := statuslineFixture(t, 8, 50)
	setAttachEnv(t, fmt.Sprintf("http://127.0.0.1:%d", port))
	mustBeAttached(t, true)
	setHome(t, "/Users/kris")
	withStdin(t, strings.NewReader(payloadJSON(t, nil)))

	var logged bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&logged)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })

	er, ew, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prevErr := os.Stderr
	os.Stderr = ew
	out := captureStdout(t, func() {
		if err := runStatusline(nil); err != nil {
			t.Errorf("runStatusline: %v", err)
		}
	})
	os.Stderr = prevErr
	ew.Close()
	errBytes, _ := io.ReadAll(er)

	if hits.Load() == 0 {
		t.Fatal("the daemon was never contacted — this test is not exercising the real path")
	}
	if out == "" {
		t.Fatal("nothing was rendered, so this test would pass by saying nothing")
	}
	if !strings.Contains(out, "🧠 34% ctx") {
		t.Fatalf("the payload was not actually consumed (%q) — a test that asserts an "+
			"absence has to prove the thing was present first", out)
	}

	for channel, text := range map[string]string{
		"stdout": out,
		"stderr": string(errBytes),
		"log":    logged.String(),
	} {
		for what, needle := range map[string]string{
			"the session id":      testSessionID,
			"the transcript path": testTranscript,
			"the transcript file": ".jsonl",
			"the cost in dollars": "12.345",
			"the project slug":    "-Users-kris-Repos-spillway",
		} {
			if strings.Contains(text, needle) {
				t.Errorf("%s carries %s (%q): %q", channel, what, needle, text)
			}
		}
	}
}
