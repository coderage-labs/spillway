package main

// `spillway statusline` prints Claude Code's statusLine hook: the session it
// is attached to on the top row, and the pool underneath — which account is
// serving, the model actually going upstream, and a headroom bar per quota
// window.
//
// Four rules shape it, all learned the hard way:
//   - Reading stdin must never block. Claude Code pipes JSON to the status
//     line, but this may be composed inside another script that already
//     consumed it, or run by hand from a terminal; blocking on a pipe nobody
//     closes freezes the status line. The read is bounded and off the
//     critical path — see statusline_session.go — and its absence degrades
//     to the pool-only line rather than failing.
//   - It must fit the terminal. A wrapped status line costs a row on every
//     render, so the line is measured in COLUMNS (not runes: emoji are two
//     columns) and trimmed lowest-value-first — see statusline_width.go.
//   - It must be fast and quiet on failure. It re-runs on every render, so a
//     stopped daemon has to cost nothing and say almost nothing — never an
//     error message. "Almost" rather than "nothing" since issue #165: a
//     session that IS proxied through a daemon that cannot answer gets one
//     dim glyph, because printing literally nothing made that case identical
//     to a session that is not on spillway at all. A session that is not on
//     spillway still prints nothing, and never even opens a connection.
//   - Colour degrades: truecolor gradient, then 256, then none, driven by the
//     environment rather than assumed.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/coderage-labs/spillway/internal/admin"
	"github.com/coderage-labs/spillway/internal/config"
)

// statusTimeout is deliberately tight: the status line re-renders constantly,
// and a slow daemon must never be felt in the prompt.
const statusTimeout = 350 * time.Millisecond

type slWindow struct {
	Name    string    `json:"name"`
	Limit   float64   `json:"limit"`
	Used    float64   `json:"used"`
	ResetAt time.Time `json:"resetAt"`
	// Expired: past its reset with nothing re-measured since (issue #135).
	// Absent from an older daemon's JSON, so it reads false there.
	Expired bool `json:"expired"`
}

type slAccount struct {
	Name      string     `json:"name"`
	Label     string     `json:"label"`
	State     string     `json:"state"`
	InFlight  int        `json:"inFlight"`
	LastModel string     `json:"lastModel"`
	Windows   []slWindow `json:"quotaWindows"`
}

func (a slAccount) display() string {
	if a.Label != "" {
		return a.Label
	}
	if i := strings.Index(a.Name, "@"); i >= 0 {
		if dom := strings.SplitN(a.Name[i+1:], ".", 2)[0]; dom != "" {
			return dom
		}
	}
	return a.Name
}

// headroom is the fraction of a window still available, or -1 when the
// provider has reported nothing yet — or when what it reported has expired
// (issue #135): either way there is no number to draw, and a stale spent
// reading must neither print a 0% bar with a countdown nor sink the
// account's ranking in pickAccount.
func (w slWindow) headroom() float64 {
	if w.Expired || w.Limit <= 0 {
		return -1
	}
	h := 1 - w.Used/w.Limit
	switch {
	case h < 0:
		return 0
	case h > 1:
		return 1
	}
	return h
}

// ── colour ───────────────────────────────────────────────────────────────
// Headroom runs full → empty, so the gradient runs cyan → amber → rust. That
// is the dashboard's palette, and low reads as danger in both.

type palette int

const (
	noColour palette = iota
	ansi256
	truecolour
)

func detectPalette() palette {
	if os.Getenv("NO_COLOR") != "" {
		return noColour
	}
	if ct := os.Getenv("COLORTERM"); strings.Contains(ct, "truecolor") || strings.Contains(ct, "24bit") {
		return truecolour
	}
	if strings.Contains(os.Getenv("TERM"), "256") {
		return ansi256
	}
	// Claude Code renders the status line itself and handles colour; assume
	// 256 rather than dropping to plain, which looks broken next to the rest.
	return ansi256
}

type rgb struct{ r, g, b int }

// gradeFor maps headroom to a colour: cyan when healthy, amber as it drops,
// rust at the spill point.
func gradeFor(h float64) rgb {
	switch {
	case h <= 0.02:
		return rgb{205, 90, 62} // rust: the pool rotates away here
	case h <= 0.20:
		return rgb{221, 162, 63} // amber
	case h <= 0.45:
		return rgb{120, 190, 190}
	default:
		return rgb{63, 198, 232} // cyan
	}
}

func (p palette) paint(c rgb, s string) string {
	switch p {
	case truecolour:
		return fmt.Sprintf("\033[38;2;%d;%d;%dm%s\033[0m", c.r, c.g, c.b, s)
	case ansi256:
		// 6x6x6 cube.
		idx := 16 + 36*(c.r*5/255) + 6*(c.g*5/255) + (c.b * 5 / 255)
		return fmt.Sprintf("\033[38;5;%dm%s\033[0m", idx, s)
	default:
		return s
	}
}

func (p palette) dim(s string) string {
	if p == noColour {
		return s
	}
	return "\033[2m" + s + "\033[0m"
}

const barCells = 8

// bar renders headroom as a filled/empty block bar, coloured by how much is
// left. Empty cells stay dim so the bar reads as a gauge, not a word.
func bar(p palette, h float64) string {
	filled := int(h*barCells + 0.5)
	if h > 0 && filled == 0 {
		filled = 1 // never show a live window as completely empty
	}
	if filled > barCells {
		filled = barCells
	}
	return p.paint(gradeFor(h), strings.Repeat("█", filled)) +
		p.dim(strings.Repeat("░", barCells-filled))
}

// slHold and slState mirror /api/state — the pool-level signals that are not
// properties of any one account.
type slHold struct {
	Count int       `json:"count"`
	Until time.Time `json:"until"`
}

type slState struct {
	Holding   *slHold    `json:"holding"`
	Usable    int        `json:"usable"`
	Total     int        `json:"total"`
	Reserve   int        `json:"reserve"`
	Overage   int        `json:"overage"`
	Exhausted int        `json:"exhausted"`
	Parked    int        `json:"parked"`
	Disabled  int        `json:"disabled"`
	InFlight  int        `json:"inFlight"`
	NextReset *time.Time `json:"nextReset"`
	// Pinned mirrors /api/state's pinned (#158): the account selection has
	// been directed at, if any. The status line needs it for two separate
	// reasons — it decides WHICH account is worth showing (a pin means every
	// next request lands there, whatever the headroom says), and it is the
	// answer to "why is only one of my accounts being used".
	Pinned string `json:"pinned"`
	// StaleCA mirrors /api/state's staleCA (issue #66): true while a MITM
	// CA regeneration this daemon run performed looks like it has left a
	// client stuck trusting the old anchor. See degraded() for how it's
	// shown.
	StaleCA bool `json:"staleCA"`
}

// getJSON is the one HTTP shape this command needs: short timeout, optional
// bearer, decode or give up quietly.
func getJSON(addr, token, path string, into any) error {
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Timeout: statusTimeout}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("admin API %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

// compactDur renders a duration the way a status line needs it: two units at
// most, no zero-padding, never "1h0m0s".
func compactDur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	// Round rather than truncate. A hold ending in 2h13m59s is "2h14m" to
	// anyone reading a prompt, and truncation makes every countdown read a
	// minute pessimistic.
	if d >= time.Minute {
		d = d.Round(time.Minute)
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()+0.5))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		h := int(d.Hours())
		if m := int(d.Minutes()) % 60; m > 0 {
			return fmt.Sprintf("%dh%dm", h, m)
		}
		return fmt.Sprintf("%dh", h)
	default:
		days := int(d.Hours()) / 24
		if h := int(d.Hours()) % 24; h > 0 {
			return fmt.Sprintf("%dd%dh", days, h)
		}
		return fmt.Sprintf("%dd", days)
	}
}

func fetchAccounts(addr, token string) ([]slAccount, error) {
	var out []slAccount
	return out, getJSON(addr, token, "/api/accounts", &out)
}

// pickAccount returns the one worth showing: the pinned account if there is
// one, else whoever is serving, else the healthiest — that is the account
// the next request will land on.
//
// The pin comes first and beats even an in-flight request (#158). Ranking by
// headroom while a pin is set answers a question nobody asked: the pool is
// not choosing, it has been told, and the next request goes to the pinned
// account however spent it is. Before this, a pin on a low-headroom account
// made the status line name a DIFFERENT account — the healthiest — which is
// worse than saying nothing about the pin at all. An in-flight request on
// some other account is the tail of whatever preceded the pin; it is
// finishing, not being chosen.
func pickAccount(list []slAccount, pinned string) *slAccount {
	if pinned != "" {
		for i := range list {
			if list[i].Name == pinned {
				return &list[i]
			}
		}
		// Pinned to a name that is in no account: fall through rather than
		// return nothing. `spillway status` reports the pin properly; the
		// prompt is better off describing the pool than going blank.
	}
	var best *slAccount
	bestHead := -2.0
	for i := range list {
		a := &list[i]
		if a.InFlight > 0 {
			return a
		}
		if a.State != "ok" {
			continue
		}
		h := 2.0
		for _, w := range a.Windows {
			if x := w.headroom(); x >= 0 && x < h {
				h = x
			}
		}
		if h == 2.0 {
			h = -1 // no signal yet
		}
		if h > bestHead {
			bestHead, best = h, a
		}
	}
	return best
}

func runStatusline(args []string) error {
	// Subcommands manage the Claude Code wiring; bare `statusline` prints the
	// line itself, which is what Claude Code invokes. A leading flag is not a
	// subcommand — `statusline --always` printed an unknown-action error into
	// the prompt.
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "install":
			return runStatuslineInstall(args[1:])
		case "uninstall":
			return runStatuslineUninstall()
		case "status":
			return runStatuslineStatus()
		default:
			return fmt.Errorf("unknown statusline action %q (install|uninstall|status)", args[0])
		}
	}

	cfg, err := config.Load()
	if err != nil {
		return nil // silent: a broken config must not garble the prompt
	}

	// Nothing at all unless this client is actually going through spillway.
	//
	// The status line is installed once into ~/.claude/settings.json and then
	// applies to every Claude Code session on the machine, including the ones
	// started without `spillway run`. Reporting the pool to those is worse
	// than saying nothing: the numbers are real but they describe traffic
	// this session is not part of, which reads as "you are on spillway" when
	// you are not.
	//
	// Claude Code passes its own environment to the status line command —
	// verified by having it run a wrapper that dumped env — so HTTPS_PROXY is
	// visible here and says exactly what the client was told to do.
	if !attachedToSpillway(cfg) && !hasFlag(args, "--always") {
		return nil
	}

	addr := cfg.Admin.Addr
	if addr == "" {
		addr = admin.DefaultAddr
	}
	token := ""
	if cfgPath, err := config.Path(); err == nil {
		if b, err := os.ReadFile(tokenPathFor(cfgPath)); err == nil {
			token = strings.TrimSpace(string(b))
		}
	}

	// Everything that can be done at once is. The whole line has one 350ms
	// budget measured from here, and the three sources of it — the two admin
	// endpoints and Claude Code's stdin payload — are independent.
	//
	// The git branch is the exception: it cannot start until the payload has
	// named a directory, so it is the one thing that is additive. It is
	// bounded twice over, by its own budget and by this deadline, and it
	// overlaps whatever is left of the admin fetches.
	deadline := time.Now().Add(statusTimeout)
	sessCh := readSessionAsync(statuslineStdin)

	var (
		list []slAccount
		st   slState
		aErr error
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		inner := make(chan struct{})
		go func() {
			defer close(inner)
			// A missing /api/state is not fatal — an older daemon still has
			// accounts to show, so the line degrades rather than disappearing.
			_ = getJSON(addr, token, "/api/state", &st)
		}()
		list, aErr = fetchAccounts(addr, token)
		<-inner
	}()

	sess := waitSession(sessCh, min(stdinBudget, time.Until(deadline)))
	if sess != nil {
		gitDeadline := time.Now().Add(gitBudget)
		if deadline.Before(gitDeadline) {
			gitDeadline = deadline
		}
		ctx, cancel := context.WithDeadline(context.Background(), gitDeadline)
		sess.Branch = gitBranch(ctx, sess.Dir)
		cancel()
	}
	<-done

	cols := terminalColumns()

	if aErr != nil {
		// The daemon did not answer within statusTimeout. Say SOMETHING
		// (issue #165).
		//
		// This used to `return nil` alongside the empty-pool case, and that
		// is what made a 400ms latency regression read as a deleted
		// feature: a status line that cannot reach the daemon was
		// byte-for-byte identical to one deliberately staying quiet because
		// the session is not proxied. The two silences were never the same
		// thing, and they are not even decided in the same place — a
		// session that is not on spillway has already returned above,
		// before any HTTP happens at all. So nothing here can make an
		// unattached session noisy; execution only reaches this line when
		// the client WAS told to proxy through spillway (or asked with
		// --always) and spillway did not answer, which is a fact worth one
		// glyph.
		//
		// One dim glyph is the whole budget. No colour, no error text, no
		// "connection refused" — those belong in `spillway status`, not in
		// a prompt that redraws on every keystroke.
		//
		// The session row still renders above it when there is one: it
		// comes from stdin, so it knows nothing about whether the daemon
		// answered, and dropping it would hide working information because
		// something unrelated broke. The marker itself is unchanged.
		p := detectPalette()
		if top := fitSession(p, sess, cols); top != "" {
			fmt.Print(top + "\n")
		}
		fmt.Print(unreachable(p))
		return nil
	}
	if len(list) == 0 {
		// Reachable, and it says there are no accounts. That is a
		// configuration state the dashboard and `spillway status` both
		// explain properly; the prompt stays out of it.
		return nil
	}
	fmt.Print(renderLine(detectPalette(), list, st, sess, time.Now(), cols))
	return nil
}

// unreachable is what the line renders when the daemon did not answer in
// time: the pool glyph the healthy line opens with, and an em dash where the
// account and the bars would be. Recognisably spillway, recognisably
// without data, and dim so it never competes with the prompt.
func unreachable(p palette) string { return p.dim("⛁ —") }

// poolTrim enumerates the reductions the pool row may apply, lowest value
// first. The order is the one agreed in #183: the spent count, then the
// in-flight count, then the fable window. The countdown after them is the
// "still does not fit" tail.
type poolTrim int

const (
	poolFull poolTrim = iota
	poolNoSpent
	poolNoInFlight
	poolNoFable
	poolNoCountdown
	poolTrimMax = poolNoCountdown
)

// windowGlyph replaces a quota window's label position with a glyph (#183).
// An unrecognised window keeps today's rendering: a wrong glyph is worse
// than none, and the name is still printed either way.
func windowGlyph(name string) string {
	switch {
	case strings.HasPrefix(name, "5h"):
		return "⏳ "
	case strings.HasPrefix(name, "7d"):
		return "📆 "
	}
	return ""
}

// renderLine is the whole status line: the session row above the pool row,
// each trimmed independently to cols. cols <= 0 means the width is unknown
// and nothing is trimmed (see terminalColumns).
func renderLine(p palette, list []slAccount, st slState, sess *slSession, now time.Time, cols int) string {
	pool := fitPool(p, list, st, sess, now, cols)
	top := fitSession(p, sess, cols)
	if top == "" {
		return pool
	}
	if pool == "" {
		return top
	}
	return top + "\n" + pool
}

// fitPool renders the pool row inside cols columns, applying the agreed
// reductions in order and truncating only if every one of them was not
// enough.
func fitPool(p palette, list []slAccount, st slState, sess *slSession, now time.Time, cols int) string {
	var out string
	for lvl := poolFull; lvl <= poolTrimMax; lvl++ {
		out = renderPool(p, list, st, sess, now, lvl)
		if cols <= 0 || displayWidth(out) <= cols {
			return out
		}
	}
	return truncateToWidth(out, cols)
}

// render builds the pool line at full detail, with no session context. The
// entry point for everything that only cares about the pool.
func render(p palette, list []slAccount, st slState, now time.Time) string {
	return renderPool(p, list, st, nil, now, poolFull)
}

// renderPool builds the pool line. Split out from runStatusline so it can be
// tested without a daemon: this is the part with the judgement in it.
//
// sess is passed in for one reason only: when the model the CLI selected and
// the model the pool last served are the same, printing both reads as a
// repetition, so the served one is dropped here and the top row carries it.
// When they differ — a mapped model, a re-routed family — both are shown,
// which is the divergence #183 exists to surface.
func renderPool(p palette, list []slAccount, st slState, sess *slSession, now time.Time, lvl poolTrim) string {
	// A parked request outranks everything else. From the client's side a
	// hold is indistinguishable from a hang, so when one is active it IS the
	// status: what is happening, and when it ends.
	if st.Holding != nil && st.Holding.Count > 0 {
		var b strings.Builder
		b.WriteString(p.paint(gradeFor(0), "⛁ held "+compactDur(st.Holding.Until.Sub(now))))
		if st.Holding.Count > 1 {
			b.WriteString(p.dim(fmt.Sprintf(" ×%d", st.Holding.Count)))
		}
		b.WriteString(p.dim(" → " + st.Holding.Until.Local().Format("15:04")))
		b.WriteString(degraded(p, st, lvl))
		return b.String()
	}

	a := pickAccount(list, st.Pinned)
	if a == nil {
		// Nothing usable and nothing parked — `exhausted: fail` mode, or the
		// credentials are gone. Say when it lifts if we know.
		out := p.paint(gradeFor(0), "⛁ pool dry")
		if st.NextReset != nil {
			out += p.dim(" → " + st.NextReset.Local().Format("15:04") +
				" (" + compactDur(st.NextReset.Sub(now)) + ")")
		}
		return out + degraded(p, st, lvl)
	}

	var b strings.Builder
	b.WriteString(p.dim("⛁ "))
	b.WriteString(a.display())

	// 📌: this account is not the pool's choice, it is the pool's
	// instruction (#158). Rotation, priority and thresholds are all
	// bypassed, which makes it the explanation for everything else on the
	// row describing a single account — the exact thing someone is looking
	// for when they wonder why only one of their accounts is being used.
	//
	// Deliberately NOT in poolTrim's reduction order. It costs three
	// columns, it appears only while a pin is set, and it is a mode rather
	// than a datum: dropping it to fit would not lose a number, it would
	// make a directed pool look like a rotating one, which is the bug.
	if st.Pinned != "" && st.Pinned == a.Name {
		b.WriteString(p.dim(" 📌"))
	}

	// 🎯 is what spillway SERVED, as against the top row's 🤖 — what the CLI
	// selected. Suppressed only when they are the same model.
	if a.LastModel != "" && !sameModel(sess, a.LastModel) {
		b.WriteString(p.dim("  🎯 "))
		b.WriteString(p.dim(shortModel(a.LastModel)))
	}

	for _, w := range a.Windows {
		h := w.headroom()
		if h < 0 {
			continue
		}
		if lvl >= poolNoFable && strings.Contains(w.Name, "fable") {
			continue
		}
		b.WriteString("  ")
		b.WriteString(p.dim(windowGlyph(w.Name)))
		b.WriteString(bar(p, h))
		b.WriteString(fmt.Sprintf(" %d%%", int(h*100+0.5)))
		b.WriteString(p.dim(" " + w.Name))
		// Once a window is nearly spent, "when does it come back" is the
		// question the percentage stops answering.
		if h <= 0.20 && !w.ResetAt.IsZero() && lvl < poolNoCountdown {
			b.WriteString(p.dim(" ↻" + compactDur(w.ResetAt.Sub(now))))
		}
	}
	if len(a.Windows) == 0 {
		b.WriteString(p.dim("  no quota signal"))
	}
	if a.InFlight > 0 && lvl < poolNoInFlight {
		b.WriteString(p.dim(fmt.Sprintf("  ✈ %d", a.InFlight)))
	}
	b.WriteString(degraded(p, st, lvl))
	return b.String()
}

// degraded appends the accounts that are not pulling their weight. Each is
// spelled out rather than abbreviated: these appear only when something is
// wrong, so being unmistakable matters more than being short.
//
// "spent" is normal operation — spillway rotating away from a used-up
// account is the product working — so it stays dim. A reserve account counts
// as spent: it will only be reached for if nothing better exists, and saying
// otherwise is what made an avoided account look healthy. "paused" and "needs
// login" are states a human put there or has to clear, and are coloured.
func degraded(p palette, st slState, lvl poolTrim) string {
	var b strings.Builder
	// Exhausted and Reserve differ only in how spillway found out — a 429
	// versus the account's own quota headers. From the prompt they are the
	// same fact: that account has nothing left.
	// Overage accounts are exhausted too, but they are reported separately
	// below — counting them here as well would double them.
	//
	// "spent" became 💀 in #183: the glyph replaces the word rather than
	// decorating it, which is what makes the emoji pay for themselves on a
	// line that was already overflowing 80 columns.
	if n := st.Exhausted - st.Overage + st.Reserve; n > 0 && lvl < poolNoSpent {
		b.WriteString(p.dim(fmt.Sprintf("  💀 %d", n)))
	}
	if st.Parked > 0 {
		b.WriteString("  " + p.paint(gradeFor(0.10), fmt.Sprintf("%d paused", st.Parked)))
	}
	if st.Disabled > 0 {
		b.WriteString("  " + p.paint(gradeFor(0), fmt.Sprintf("⚠ %d needs login", st.Disabled)))
	}
	// A replaced MITM CA (issue #66): this session may be one of the ones
	// it stranded. Restarting is the only fix (NODE_EXTRA_CA_CERTS is read
	// once, at process start), so the warning names it outright rather
	// than describing the symptom.
	if st.StaleCA {
		b.WriteString("  " + p.paint(gradeFor(0), "⚠ stale CA — restart this session"))
	}
	// The only state that costs money. It is deliberately the last thing on
	// the line and the only one that names a currency: everything else here
	// is about waiting, this one is about a bill.
	if st.Overage > 0 {
		b.WriteString("  " + p.paint(gradeFor(0), fmt.Sprintf("£ %d on extra usage", st.Overage)))
	}
	return b.String()
}

// shortModel trims the noise: version suffixes and the vendor prefix.
func shortModel(m string) string {
	m = strings.TrimPrefix(m, "claude-")
	if i := strings.LastIndex(m, "-20"); i > 0 && len(m)-i == 9 {
		m = m[:i]
	}
	return m
}

// ── install ──────────────────────────────────────────────────────────────

// selfPath is the absolute path to this binary. The status line runs with a
// minimal environment, so a bare command name is unreliable — GOPATH/bin is
// frequently absent from that PATH, and the failure is silent (an empty line).
// A variable so the service integration tests can point it at a real
// spillway binary: under `go test` os.Args[0] is the test binary, and
// installing a service that runs it would register the test suite as a
// daemon.
var selfPath = func() (string, error) {
	// Deliberately does NOT resolve symlinks.
	//
	// It used to. A package manager installs a stable symlink into a bin
	// directory and points it at a versioned path — Homebrew's
	// /opt/homebrew/bin/spillway -> Caskroom/spillway/0.1.1/spillway — and
	// resolving through it records a path that the next upgrade deletes. The
	// launchd job and the Claude Code status line both bake this in, so both
	// would break at the next release, silently and later.
	//
	// Prefer the name we were invoked as, looked up on PATH. LookPath returns
	// an absolute argv[0] unchanged and finds a bare name in the bin
	// directory, which is the stable one either way.
	if p, err := exec.LookPath(os.Args[0]); err == nil {
		if abs, aerr := filepath.Abs(p); aerr == nil {
			if _, serr := os.Stat(abs); serr == nil {
				return abs, nil
			}
		}
	}
	// No usable argv[0] (launchd re-exec, a stripped environment): fall back
	// to whatever the OS reports, absolute but possibly already resolved.
	return os.Executable()
}

func statuslineCommand() (string, error) {
	p, err := selfPath()
	if err != nil {
		return "", err
	}
	return p + " statusline", nil
}

// isOurs reports whether an existing statusLine entry is one we installed,
// so uninstall never removes someone else's.
func isOurs(cmd string) bool {
	return strings.Contains(cmd, "spillway statusline")
}

func runStatuslineInstall(args []string) error {
	force := false
	for _, a := range args {
		if a == "--force" || a == "-f" {
			force = true
		}
	}
	path, err := claudeSettingsPath()
	if err != nil {
		return err
	}
	settings, err := readSettings(path)
	if err != nil {
		return err
	}

	// Never silently replace someone else's status line — it may be doing a
	// lot more than ours.
	if cur, ok := settings["statusLine"].(map[string]any); ok && !force {
		existing, _ := cur["command"].(string)
		if !isOurs(existing) {
			return fmt.Errorf("a different status line is already installed:\n  %s\n"+
				"re-run with --force to replace it (the previous settings.json is kept as %s)",
				existing, filepath.Base(path)+".spillway.bak")
		}
	}

	cmd, err := statuslineCommand()
	if err != nil {
		return err
	}
	settings["statusLine"] = map[string]any{
		"type":    "command",
		"command": cmd,
		"padding": 0,
	}
	if err := writeSettings(path, settings); err != nil {
		return err
	}
	fmt.Printf("status line installed in %s\n  %s\n", path, cmd)
	fmt.Println("takes effect in new Claude Code sessions")
	return nil
}

func runStatuslineUninstall() error {
	path, err := claudeSettingsPath()
	if err != nil {
		return err
	}
	settings, err := readSettings(path)
	if err != nil {
		return err
	}
	cur, ok := settings["statusLine"].(map[string]any)
	if !ok {
		fmt.Println("no status line configured")
		return nil
	}
	existing, _ := cur["command"].(string)
	if !isOurs(existing) {
		return fmt.Errorf("the installed status line is not spillway's, leaving it alone:\n  %s", existing)
	}
	delete(settings, "statusLine")
	if err := writeSettings(path, settings); err != nil {
		return err
	}
	fmt.Printf("status line removed from %s\n", path)
	return nil
}

func runStatuslineStatus() error {
	path, err := claudeSettingsPath()
	if err != nil {
		return err
	}
	settings, err := readSettings(path)
	if err != nil {
		return err
	}
	cur, ok := settings["statusLine"].(map[string]any)
	if !ok {
		fmt.Printf("status line NOT installed (%s)\n", path)
		return nil
	}
	existing, _ := cur["command"].(string)
	who := "another tool's"
	if isOurs(existing) {
		who = "spillway's"
	}
	fmt.Printf("status line installed (%s), %s:\n  %s\n", path, who, existing)
	return nil
}

// attachedToSpillway reports whether the process that invoked us was pointed
// at this spillway, by either supported attach path.
func attachedToSpillway(cfg *config.Config) bool {
	host := cfg.Proxy.Host
	if host == "" {
		host = config.DefaultProxyHost
	}
	port := cfg.Proxy.Port
	if port == 0 {
		port = config.DefaultProxyPort
	}
	want := net.JoinHostPort(host, strconv.Itoa(port))

	for _, k := range []string{
		// MITM mode, which is what `spillway run` sets.
		"HTTPS_PROXY", "https_proxy",
		// The base-URL route, for a client attached that way instead.
		"ANTHROPIC_BASE_URL",
	} {
		if hostPortOf(os.Getenv(k)) == want {
			return true
		}
	}
	return false
}

// hostPortOf reduces a proxy or base URL to host:port, so a trailing slash or
// a missing scheme cannot make an equal address compare unequal.
func hostPortOf(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if !strings.Contains(v, "//") {
		v = "//" + v
	}
	u, err := url.Parse(v)
	if err != nil || u.Host == "" {
		return ""
	}
	if u.Port() == "" {
		switch u.Scheme {
		case "https":
			return net.JoinHostPort(u.Hostname(), "443")
		default:
			return net.JoinHostPort(u.Hostname(), "80")
		}
	}
	return net.JoinHostPort(u.Hostname(), u.Port())
}

func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == name {
			return true
		}
	}
	return false
}
