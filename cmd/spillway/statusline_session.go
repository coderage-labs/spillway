package main

// The session half of the status line (issue #183).
//
// Claude Code pipes a JSON payload to the status line command on every
// render and spillway used to discard all of it, so the line said what the
// pool was doing and nothing about the session attached to it. This reads
// that payload and renders the top row:
//
//	📁 ~/Repos/spillway  🌿 main  🤖 Opus 5 · high  🧠 34% ctx  📊 128k (2.1k tpm)
//
// Three rules, all of them about not making the line worse:
//
//   - Reading stdin must never be able to block the render. The comment at
//     the top of statusline.go used to say this command must NOT read stdin
//     at all, because a wrapper script may have consumed it already and a
//     read on a pipe nobody closes hangs the prompt forever. That reason is
//     still true; the fix is a bounded read on a goroutine, not a refusal.
//     Absent, empty, malformed or never-closed stdin all degrade to the
//     pool-only line.
//   - §5: nothing identifying may reach a log or the request database. The
//     payload carries session_id, transcript_path, a session name and real
//     cost figures. None of them are declared in slPayload, so the decoder
//     drops them on the floor: they are never in a variable that something
//     could later print. Only derived numbers leave this file.
//   - The git branch is the only shellout on the line. It is bounded by its
//     own budget AND by the overall 350 ms deadline, whichever is tighter.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// stdinBudget is what the payload read gets. It runs concurrently with the
// admin fetches, so it is not additive with them — but it is additive with
// the git shellout that depends on its result, which is why it is small.
// Claude Code writes the payload and closes the pipe before the command is
// even scheduled, so the normal case returns immediately.
const stdinBudget = 80 * time.Millisecond

// gitBudget bounds the one shellout. `git rev-parse` on a warm repository is
// a couple of milliseconds; this is for the cold, the networked and the
// enormous, where the right answer is no branch rather than no status line.
const gitBudget = 120 * time.Millisecond

// maxPayload caps the stdin read. Nothing legitimate is near this; it exists
// so a harness piping something endless cannot make the line allocate until
// the process dies.
const maxPayload = 1 << 20

// statuslineStdin is where the payload is read from. A variable so tests can
// supply one without a pipe; production is os.Stdin.
var statuslineStdin io.Reader = os.Stdin

// slPayload is the subset of Claude Code's status line payload this command
// uses. The field names are observed from a real payload (CLI 2.1.274), not
// inferred.
//
// Deliberately absent, and it must stay that way: session_id,
// transcript_path, the session name, cost.total_cost_usd. encoding/json
// ignores what is not declared, so §5 is enforced by the shape of this
// struct rather than by remembering not to print something.
type slPayload struct {
	Model struct {
		DisplayName string `json:"display_name"`
		ID          string `json:"id"`
	} `json:"model"`
	Effort struct {
		Level string `json:"level"`
	} `json:"effort"`
	Workspace struct {
		CurrentDir string `json:"current_dir"`
	} `json:"workspace"`
	Cwd           string `json:"cwd"`
	ContextWindow struct {
		// A pointer: 0% used and "the field was not sent" are different
		// facts, and only one of them is worth a segment.
		UsedPercentage   *float64 `json:"used_percentage"`
		TotalInputTokens int64    `json:"total_input_tokens"`
		CurrentUsage     struct {
			InputTokens              int64 `json:"input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		} `json:"current_usage"`
	} `json:"context_window"`
	Cost struct {
		TotalDurationMS int64 `json:"total_duration_ms"`
	} `json:"cost"`
}

// slSession is what survives the payload: derived numbers and two names.
type slSession struct {
	Dir     string // workspace.current_dir, falling back to cwd
	Branch  string // filled in after decoding, by gitBranch
	Model   string // model.display_name — what the CLI selected
	ModelID string // model.id, for comparing against what the pool served
	Effort  string // effort.level

	CtxPct    float64
	HasCtx    bool
	Tokens    int64
	TPM       float64
	HasTPM    bool
	HasTokens bool
}

// minSessionForRate is the session duration below which a tokens-per-minute
// figure is arithmetic rather than information: a 4-second-old session
// divides a real token count by a fraction and reports a rate nobody will
// ever sustain.
const minSessionForRate = 10 * time.Second

func decodeSession(b []byte) *slSession {
	b = []byte(strings.TrimSpace(string(b)))
	if len(b) == 0 || b[0] != '{' {
		// Not an object: a harness piping a log line, a bare string, or
		// nothing at all. Quiet degrade, no error, no guessing.
		return nil
	}
	var p slPayload
	if err := json.Unmarshal(b, &p); err != nil {
		return nil
	}

	s := &slSession{
		Dir:     p.Workspace.CurrentDir,
		Model:   sanitiseField(p.Model.DisplayName),
		ModelID: sanitiseField(p.Model.ID),
		Effort:  sanitiseField(p.Effort.Level),
	}
	if s.Dir == "" {
		s.Dir = p.Cwd
	}
	s.Dir = sanitiseField(s.Dir)

	if p.ContextWindow.UsedPercentage != nil {
		s.CtxPct, s.HasCtx = *p.ContextWindow.UsedPercentage, true
		if s.CtxPct < 0 {
			s.CtxPct = 0
		}
		if s.CtxPct > 100 {
			s.CtxPct = 100
		}
	}

	// Cumulative for the session, which is NOT the resident context in
	// current_usage. current_usage is the fallback only because an older CLI
	// may not send the total, and one number is better than none.
	s.Tokens = p.ContextWindow.TotalInputTokens
	if s.Tokens <= 0 {
		u := p.ContextWindow.CurrentUsage
		s.Tokens = u.InputTokens + u.OutputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
	}
	s.HasTokens = s.Tokens > 0

	if d := time.Duration(p.Cost.TotalDurationMS) * time.Millisecond; s.HasTokens && d >= minSessionForRate {
		s.TPM, s.HasTPM = float64(s.Tokens)/d.Minutes(), true
	}

	if s.Dir == "" && s.Model == "" && !s.HasCtx && !s.HasTokens {
		return nil // an object, but nothing this line can say
	}
	return s
}

// sanitiseField makes a payload string safe to put on a status line. Control
// characters are the whole risk: a newline in a model name or a workspace
// path would add a row to the prompt, and an escape sequence would repaint
// the terminal. Both are stripped rather than rejected.
func sanitiseField(s string) string {
	return strings.Map(func(r rune) rune {
		if r == 0x1b || unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
}

// readSessionAsync starts the payload read immediately and hands back a
// channel that yields at most one session. The caller decides how long to
// wait; nothing here can outlive the process.
func readSessionAsync(r io.Reader) <-chan *slSession {
	ch := make(chan *slSession, 1) // buffered: a timed-out send must not leak
	if r == nil {
		close(ch)
		return ch
	}
	go func() {
		defer close(ch)
		// A character device is a terminal, not a payload. Reading it would
		// block until the user typed something, which is the freeze the
		// original "do not read stdin" rule was written to avoid — and it is
		// also what `spillway statusline` run by hand looks like.
		if f, ok := r.(*os.File); ok {
			st, err := f.Stat()
			if err != nil || st.Mode()&os.ModeCharDevice != 0 {
				return
			}
		}
		b, err := io.ReadAll(io.LimitReader(r, maxPayload))
		if err != nil && len(b) == 0 {
			return
		}
		if s := decodeSession(b); s != nil {
			ch <- s
		}
	}()
	return ch
}

// waitSession takes the payload if it arrives inside budget, and otherwise
// gives up on it. Degrade, never delay.
func waitSession(ch <-chan *slSession, budget time.Duration) *slSession {
	if budget <= 0 {
		return nil
	}
	t := time.NewTimer(budget)
	defer t.Stop()
	select {
	case s := <-ch:
		return s
	case <-t.C:
		return nil
	}
}

// gitBranch is the only shellout on the line, and the only thing here that
// can cost real time. It is bounded by ctx, which the caller ties to the
// overall 350 ms deadline. A detached HEAD, a non-repository or a timeout
// all return "" — the branch is a droppable segment, so its absence costs
// nothing.
func gitBranch(ctx context.Context, dir string) string {
	if dir == "" || ctx.Err() != nil {
		return ""
	}
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD")
	// GIT_OPTIONAL_LOCKS=0 keeps a status line render from taking the index
	// lock out from under an interactive git in the same repository.
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	b := sanitiseField(strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0])
	if b == "HEAD" {
		return "" // detached: there is no branch to name
	}
	return b
}

// ── the top row ──────────────────────────────────────────────────────────

// tildePath shortens a home-relative path the way a prompt does.
//
// Both sides are normalised to forward slashes BEFORE comparing. Comparing
// raw against `home + os.PathSeparator` looks right and is wrong on Windows,
// where the payload's path may use either separator: the prefix silently
// fails to match, the whole path renders, and the row is nine columns wider
// than every width test on a developer machine says it is.
func tildePath(dir string) string {
	d := filepath.ToSlash(dir)
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if h := strings.TrimSuffix(filepath.ToSlash(home), "/"); h != "" {
			if d == h {
				return "~"
			}
			if strings.HasPrefix(d, h+"/") {
				return "~/" + d[len(h)+1:]
			}
		}
	}
	return d
}

// elidePath drops the middle of a path, keeping the root marker and the
// directory actually being worked in: ~/Repos/spillway → ~/spillway.
func elidePath(p string) string {
	parts := strings.Split(p, "/")
	if len(parts) <= 2 {
		return p
	}
	return parts[0] + "/" + parts[len(parts)-1]
}

// compactCount renders a token count the way a status line needs it: three
// significant figures at most, and never more than five columns.
func compactCount(n float64) string {
	switch {
	case n < 0:
		return "0"
	case n < 1000:
		return fmt.Sprintf("%d", int(n+0.5))
	case n < 10000:
		return fmt.Sprintf("%.1fk", n/1000)
	case n < 1000000:
		return fmt.Sprintf("%dk", int(n/1000+0.5))
	case n < 10000000:
		return fmt.Sprintf("%.1fM", n/1000000)
	default:
		return fmt.Sprintf("%dM", int(n/1000000+0.5))
	}
}

// sessionTrim enumerates the reductions the top row may apply, lowest value
// first. The order is the one agreed in #183: tokens-per-minute, then the
// git branch, then the path is abbreviated. The three after that are the
// "still does not fit" tail — a 40-column terminal has to give up something
// the issue never had to name.
type sessionTrim int

const (
	sessFull sessionTrim = iota
	sessNoTPM
	sessNoBranch
	sessShortPath
	sessNoTokens
	sessNoPath
	sessNoEffort
	sessTrimMax = sessNoEffort
)

// renderSession builds the top row at a given reduction level. Returns ""
// when there is nothing worth a row.
func renderSession(p palette, s *slSession, lvl sessionTrim) string {
	if s == nil {
		return ""
	}
	var segs []string

	if s.Dir != "" && lvl < sessNoPath {
		d := tildePath(s.Dir)
		if lvl >= sessShortPath {
			d = elidePath(d)
		}
		segs = append(segs, p.dim("📁 ")+d)
	}
	if s.Branch != "" && lvl < sessNoBranch {
		segs = append(segs, p.dim("🌿 ")+s.Branch)
	}
	if s.Model != "" {
		m := p.dim("🤖 ") + s.Model
		if s.Effort != "" && lvl < sessNoEffort {
			m += p.dim(" · " + s.Effort)
		}
		segs = append(segs, m)
	}
	if s.HasCtx {
		// Coloured by what is LEFT, so it shares the gradient with the
		// headroom bars below it: cyan with room, rust near compaction.
		pct := fmt.Sprintf("%d%%", int(s.CtxPct+0.5))
		segs = append(segs, p.dim("🧠 ")+p.paint(gradeFor(1-s.CtxPct/100), pct)+p.dim(" ctx"))
	}
	if s.HasTokens && lvl < sessNoTokens {
		t := p.dim("📊 ") + compactCount(float64(s.Tokens))
		if s.HasTPM && lvl < sessNoTPM {
			t += p.dim(fmt.Sprintf(" (%s tpm)", compactCount(s.TPM)))
		}
		segs = append(segs, t)
	}
	return strings.Join(segs, "  ")
}

// fitSession renders the top row inside cols columns, applying the agreed
// reductions in order and truncating only if every one of them was not
// enough. cols <= 0 means the width is unknown, which means no trimming —
// see terminalColumns.
func fitSession(p palette, s *slSession, cols int) string {
	var out string
	for lvl := sessFull; lvl <= sessTrimMax; lvl++ {
		out = renderSession(p, s, lvl)
		if out == "" || cols <= 0 || displayWidth(out) <= cols {
			return out
		}
	}
	return truncateToWidth(out, cols)
}

// modelKey normalises a model name for comparison, so "Opus 5" from the CLI
// and "claude-opus-5-20260114" from the pool are recognisably the same
// model. Comparison only — nothing rendered goes through it.
func modelKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.NewReplacer(" ", "-", ".", "-", "_", "-").Replace(s)
	return shortModel(s)
}

// sameModel reports whether the model the CLI selected and the model the
// pool last served are the same one. When they are, showing both is a
// repetition; when they differ, that difference is the thing #183 exists to
// surface.
func sameModel(s *slSession, served string) bool {
	if s == nil || served == "" {
		return false
	}
	k := modelKey(served)
	if k == "" {
		return false
	}
	return k == modelKey(s.Model) || k == modelKey(s.ModelID)
}
