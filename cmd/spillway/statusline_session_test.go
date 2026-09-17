package main

// Issue #183: the status line said what the pool was doing and nothing about
// the session attached to it, because `runStatusline` threw away the JSON
// payload Claude Code pipes it on every render.
//
// What these drive:
//   - the payload's observed fields become the top row;
//   - absent, empty, malformed and never-closed stdin all degrade to the
//     pool-only line rather than failing or hanging;
//   - the two model names render separately when they differ and collapse
//     when they match — the divergence is the point of the issue;
//   - the rendered COLUMN width never exceeds the terminal, and what gets
//     dropped to achieve that is dropped in the agreed order;
//   - §5: nothing identifying from the payload reaches stdout or a log.

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The needles. A real payload carries all three and none of them may ever
// appear in output or in a log line.
const (
	testSessionID  = "9f3c1e2a-4b7d-4f19-8c2e-1a2b3c4d5e6f"
	testTranscript = "/Users/kris/.claude/projects/-Users-kris-Repos-spillway/9f3c1e2a-4b7d-4f19-8c2e-1a2b3c4d5e6f.jsonl"
	testCostUSD    = 12.3456789
)

// payloadJSON builds a payload in the shape Claude Code 2.1.274 actually
// sends — including the fields spillway must ignore. Built here rather than
// committed as a fixture: a real capture carries a real session id, a real
// path and a real bill.
func payloadJSON(t *testing.T, mutate func(map[string]any)) string {
	t.Helper()
	m := map[string]any{
		"session_id":      testSessionID,
		"transcript_path": testTranscript,
		"cwd":             "/Users/kris/fallback-cwd",
		"version":         "2.1.274",
		"model":           map[string]any{"display_name": "Opus 5", "id": "claude-opus-5"},
		"effort":          map[string]any{"level": "high"},
		"workspace":       map[string]any{"current_dir": "/Users/kris/Repos/spillway"},
		"thinking":        map[string]any{"enabled": true},
		"output_style":    map[string]any{"name": "default"},
		"fast_mode":       false,
		"context_window": map[string]any{
			"used_percentage":     34,
			"context_window_size": 1000000,
			"total_input_tokens":  128000,
			"current_usage": map[string]any{
				"input_tokens": 12, "output_tokens": 34,
				"cache_creation_input_tokens": 4165, "cache_read_input_tokens": 1816,
			},
		},
		"cost": map[string]any{
			"total_cost_usd": testCostUSD, "total_duration_ms": 3657000,
			"total_lines_added": 210, "total_lines_removed": 44,
		},
		"rate_limits": map[string]any{
			"five_hour": map[string]any{"used_percentage": 9},
			"seven_day": map[string]any{"used_percentage": 89},
		},
		"prompt_cache": map[string]any{"hit_ratio": 0.93, "requests": 41, "misses": 3},
	}
	if mutate != nil {
		mutate(m)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// setHome pins the home directory so ~-abbreviation is the same everywhere
// the tests run, rather than depending on whose machine it is.
func setHome(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // windows
}

// withStdin points the status line's payload reader at s.
func withStdin(t *testing.T, r io.Reader) {
	t.Helper()
	prev := statuslineStdin
	statuslineStdin = r
	t.Cleanup(func() { statuslineStdin = prev })
}

// sessionFrom decodes a payload through the real stdin path — the goroutine,
// the budget and the decoder — rather than calling decodeSession directly,
// so a change that breaks the plumbing shows up here too.
func sessionFrom(t *testing.T, payload string) *slSession {
	t.Helper()
	return waitSession(readSessionAsync(strings.NewReader(payload)), time.Second)
}

// ── the payload ──────────────────────────────────────────────────────────

func TestDecodeSessionReadsTheObservedFields(t *testing.T) {
	s := sessionFrom(t, payloadJSON(t, nil))
	if s == nil {
		t.Fatal("a real payload decoded to nothing")
	}
	if s.Dir != "/Users/kris/Repos/spillway" {
		t.Errorf("Dir = %q, want workspace.current_dir", s.Dir)
	}
	if s.Model != "Opus 5" || s.ModelID != "claude-opus-5" {
		t.Errorf("model = %q/%q, want Opus 5/claude-opus-5", s.Model, s.ModelID)
	}
	if s.Effort != "high" {
		t.Errorf("Effort = %q, want high — effort.level IS on the payload, it is not inferred", s.Effort)
	}
	if !s.HasCtx || s.CtxPct != 34 {
		t.Errorf("context = %v/%v, want 34%% — context_window.used_percentage is handed "+
			"over directly and must never be reconstructed from the transcript", s.HasCtx, s.CtxPct)
	}
	if !s.HasTokens || s.Tokens != 128000 {
		t.Errorf("Tokens = %d, want 128000 (context_window.total_input_tokens)", s.Tokens)
	}
	// 128000 tokens over 3_657_000ms = 60.95 minutes = ~2100/min.
	if !s.HasTPM || s.TPM < 2090 || s.TPM > 2110 {
		t.Errorf("TPM = %v, want ~2100", s.TPM)
	}
}

// workspace.current_dir is the authority; cwd is the fallback, and one of
// them is always present.
func TestDecodeSessionFallsBackToCwd(t *testing.T) {
	s := sessionFrom(t, payloadJSON(t, func(m map[string]any) {
		m["workspace"] = map[string]any{}
	}))
	if s == nil || s.Dir != "/Users/kris/fallback-cwd" {
		t.Fatalf("session = %+v, want Dir from cwd", s)
	}
}

// 0% used and "the CLI did not send the field" are different facts, and only
// one of them earns a segment.
func TestDecodeSessionDistinguishesZeroContextFromAbsent(t *testing.T) {
	zero := sessionFrom(t, payloadJSON(t, func(m map[string]any) {
		m["context_window"].(map[string]any)["used_percentage"] = 0
	}))
	if zero == nil || !zero.HasCtx || zero.CtxPct != 0 {
		t.Errorf("0%% context decoded as %+v, want present and zero", zero)
	}
	absent := sessionFrom(t, payloadJSON(t, func(m map[string]any) {
		delete(m["context_window"].(map[string]any), "used_percentage")
	}))
	if absent == nil || absent.HasCtx {
		t.Errorf("absent used_percentage decoded as %+v, want no context segment", absent)
	}
	if got := renderSession(noColour, absent, sessFull); strings.Contains(got, "🧠") {
		t.Errorf("row %q shows a context segment the payload never sent", got)
	}
}

// A four-second-old session divides a real token count by a fraction of a
// minute and reports a rate nobody will sustain. No rate is better.
func TestDecodeSessionOmitsRateForAVeryYoungSession(t *testing.T) {
	s := sessionFrom(t, payloadJSON(t, func(m map[string]any) {
		m["cost"].(map[string]any)["total_duration_ms"] = 4000
	}))
	if s == nil || s.HasTPM {
		t.Errorf("session = %+v, want no tokens-per-minute for a 4s session", s)
	}
	if got := renderSession(noColour, s, sessFull); strings.Contains(got, "tpm") {
		t.Errorf("row %q invented a rate from four seconds of session", got)
	}
}

// Stdin arrives from Claude Code, from a wrapper script that already read
// it, from a test harness and from a human running the command by hand.
// Every one of those that is not a payload must degrade to the pool-only
// line — never an error, never a hang, never a guess.
func TestStdinDegradesWhenItIsNotAPayload(t *testing.T) {
	for name, in := range map[string]string{
		"empty":            "",
		"whitespace":       "   \n\t ",
		"truncated json":   `{"model":{"display_name":"Opus 5"`,
		"not json at all":  "spillway statusline\n",
		"a bare string":    `"hello"`,
		"an array":         `[1,2,3]`,
		"an empty object":  `{}`,
		"nothing usable":   `{"version":"2.1.274","session_id":"x"}`,
		"wrong types":      `{"model":{"display_name":42},"context_window":{"used_percentage":"lots"}}`,
		"null everything":  `{"model":null,"workspace":null,"context_window":null,"cost":null}`,
		"nul bytes inside": "\x00\x00\x00",
	} {
		if s := sessionFrom(t, in); s != nil {
			t.Errorf("%s decoded to %+v, want nothing — the line must fall back to the pool only", name, s)
		}
	}
	// And an absent stdin entirely.
	if s := waitSession(readSessionAsync(nil), time.Second); s != nil {
		t.Errorf("a nil stdin decoded to %+v, want nothing", s)
	}
}

// A control character in a payload string would add a row to the prompt or
// repaint the terminal. The payload is untrusted input.
func TestPayloadStringsAreStrippedOfControlCharacters(t *testing.T) {
	s := sessionFrom(t, payloadJSON(t, func(m map[string]any) {
		m["model"] = map[string]any{"display_name": "Opus\n5\x1b[31m", "id": "claude-opus-5"}
		m["workspace"] = map[string]any{"current_dir": "/tmp/pro\nject"}
	}))
	if s == nil {
		t.Fatal("payload decoded to nothing")
	}
	row := renderSession(noColour, s, sessFull)
	if strings.ContainsAny(row, "\n\x1b") {
		t.Errorf("row %q carries a control character straight from the payload", row)
	}
}

// The path abbreviation must not depend on which separator the payload
// happens to use, and must not depend on which platform the tests run on.
// A separator mismatch does not fail loudly: it renders the whole path, and
// the row comes out nine columns wider than every width test says it is —
// on Windows only, where nobody looks.
func TestTildePathHandlesEitherSeparator(t *testing.T) {
	setHome(t, "/Users/kris")
	for in, want := range map[string]string{
		"/Users/kris/Repos/spillway": "~/Repos/spillway",
		// The same path spelled in this platform's own separator — a
		// backslash one on Windows, where the home comparison used to be
		// done against os.PathSeparator and silently never matched.
		filepath.FromSlash("/Users/kris/Repos/spillway"): "~/Repos/spillway",
		"/Users/kris":                 "~",
		"/Users/kristina/Repos/x":     "/Users/kristina/Repos/x", // not a prefix match
		"/opt/elsewhere":              "/opt/elsewhere",
		"/Users/kris/Repos/coderage/": "~/Repos/coderage/",
	} {
		if got := tildePath(in); got != want {
			t.Errorf("tildePath(%q) = %q, want %q", in, got, want)
		}
	}
	// And the abbreviation applied when the row still will not fit.
	for in, want := range map[string]string{
		"~/Repos/coderage/spillway": "~/spillway",
		"~/spillway":                "~/spillway",
		"/a/b/c":                    "/c",
		"~":                         "~",
	} {
		if got := elidePath(in); got != want {
			t.Errorf("elidePath(%q) = %q, want %q", in, got, want)
		}
	}
}

// ── the git branch ───────────────────────────────────────────────────────

// The one shellout. Every way it can fail returns no branch, because the
// branch is a droppable segment and a missing one costs nothing — whereas a
// blocked one costs the whole status line.
func TestGitBranchFailsQuiet(t *testing.T) {
	ctx := context.Background()
	if got := gitBranch(ctx, ""); got != "" {
		t.Errorf("gitBranch with no directory = %q, want empty", got)
	}
	if got := gitBranch(ctx, t.TempDir()); got != "" {
		t.Errorf("gitBranch outside a repository = %q, want empty", got)
	}
	// An already-expired deadline must not even start the process — asserted
	// against a repository that genuinely HAS a branch to find, or the
	// deadline would not be what made the answer empty.
	repo := newGitRepo(t, "deadline-test-branch")
	if got := gitBranch(ctx, repo); got != "deadline-test-branch" {
		t.Fatalf("gitBranch(%q) = %q — the fixture has no branch, so the deadline "+
			"case below would pass for the wrong reason", repo, got)
	}
	dead, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if got := gitBranch(dead, repo); got != "" {
		t.Errorf("gitBranch past its deadline = %q, want empty", got)
	}
}

// And the success path, against a repository built for it.
//
// NOT against the repository the tests run from: actions/checkout leaves a
// PR build on a detached HEAD, so that version of this test asserted "some
// branch exists" and failed in CI while passing on every developer machine.
// The branch name is chosen here, so the assertion is on the value rather
// than on its mere presence.
func TestGitBranchNamesTheBranch(t *testing.T) {
	dir := newGitRepo(t, "spillway-test-branch")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if got := gitBranch(ctx, dir); got != "spillway-test-branch" {
		t.Errorf("gitBranch(%q) = %q, want spillway-test-branch", dir, got)
	}

	// A detached HEAD has no branch to name — which is exactly what a PR
	// build looks like, and it must render nothing rather than "HEAD".
	git(t, dir, "checkout", "--detach")
	if got := gitBranch(ctx, dir); got != "" {
		t.Errorf("gitBranch on a detached HEAD = %q, want empty", got)
	}
}

// newGitRepo makes a throwaway repository on branch name, with one commit so
// HEAD resolves, and with the ambient git configuration shut out.
func newGitRepo(t *testing.T, name string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git is not installed, so the shellout cannot be exercised: %v", err)
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "root")
	git(t, dir, "branch", "-M", name)
	return dir
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		// A developer's global hooks, templates or commit signing must not
		// decide whether this test passes.
		"GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "gitconfig"),
		"GIT_CONFIG_SYSTEM="+filepath.Join(t.TempDir(), "gitconfig"),
		"GIT_AUTHOR_NAME=spillway", "GIT_AUTHOR_EMAIL=spillway@example.invalid",
		"GIT_COMMITTER_NAME=spillway", "GIT_COMMITTER_EMAIL=spillway@example.invalid",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// ── the two models ───────────────────────────────────────────────────────

// The case #183 exists for: the session is on Opus 5 while the pool last
// served haiku-4-5. Both are true, they are different facts, and today's
// line showed only the second one in a position that read as the first.
func TestBothModelsRenderWhenTheyDiffer(t *testing.T) {
	setHome(t, "/Users/kris")
	s := sessionFrom(t, payloadJSON(t, nil))
	a := okAccount()
	a.LastModel = "claude-haiku-4-5"

	line := renderLine(noColour, []slAccount{a}, slState{Total: 1, Usable: 1}, s, time.Now(), 0)
	if !strings.Contains(line, "🤖 Opus 5") {
		t.Errorf("line = %q, want the model the CLI selected", line)
	}
	if !strings.Contains(line, "🎯 haiku-4-5") {
		t.Errorf("line = %q, want the model spillway served", line)
	}
	// Distinguishable, on their own rows: 🤖 above, 🎯 below.
	rows := strings.Split(line, "\n")
	if len(rows) != 2 {
		t.Fatalf("line rendered as %d rows, want 2: %q", len(rows), line)
	}
	if !strings.Contains(rows[0], "🤖") || strings.Contains(rows[0], "🎯") {
		t.Errorf("top row %q should carry 🤖 and only 🤖", rows[0])
	}
	if !strings.Contains(rows[1], "🎯") || strings.Contains(rows[1], "🤖") {
		t.Errorf("pool row %q should carry 🎯 and only 🎯", rows[1])
	}
}

// When they are the same model, printing it twice is a repetition — so the
// served copy goes and the top row carries it. Including across the spelling
// difference between a display name and a dated wire id.
func TestTheModelsCollapseWhenTheyMatch(t *testing.T) {
	setHome(t, "/Users/kris")
	for _, served := range []string{"claude-opus-5", "claude-opus-5-20260114", "Opus 5"} {
		s := sessionFrom(t, payloadJSON(t, nil))
		a := okAccount()
		a.LastModel = served

		line := renderLine(noColour, []slAccount{a}, slState{Total: 1, Usable: 1}, s, time.Now(), 0)
		if strings.Contains(line, "🎯") {
			t.Errorf("served %q: line = %q, want the served model collapsed away", served, line)
		}
		if !strings.Contains(line, "🤖 Opus 5") {
			t.Errorf("served %q: line = %q, collapsing lost the model entirely", served, line)
		}
	}
	// A near miss is not a match: opus-5 and opus-4-5 must both show.
	s := sessionFrom(t, payloadJSON(t, nil))
	a := okAccount()
	a.LastModel = "claude-opus-4-5"
	if line := renderLine(noColour, []slAccount{a}, slState{Total: 1, Usable: 1}, s, time.Now(), 0); !strings.Contains(line, "🎯") {
		t.Errorf("line = %q, want opus-4-5 shown alongside Opus 5 — they are different models", line)
	}
}

// Without a payload there is no CLI model to compare against, so the pool
// row keeps showing what it served. Nothing about today's line depends on
// stdin arriving.
func TestServedModelStillShowsWithoutAPayload(t *testing.T) {
	line := renderLine(noColour, []slAccount{okAccount()}, slState{Total: 1, Usable: 1}, nil, time.Now(), 0)
	if !strings.Contains(line, "🎯 haiku-4-5") {
		t.Errorf("line = %q, want the served model on a pool-only line", line)
	}
	if strings.Contains(line, "\n") {
		t.Errorf("line = %q, want a single row when there is no session payload", line)
	}
}
