package main

// `spillway statusline` prints one line for Claude Code's statusLine hook:
// which account is serving, the model actually going upstream, and a headroom
// bar per quota window.
//
// Three rules shape it, all learned the hard way:
//   - It must NOT read stdin. Claude Code pipes JSON to the status line, but
//     this may be composed inside another script that already consumed it;
//     blocking on a closed pipe freezes the status line.
//   - It must be fast and silent on failure. It re-runs on every render, so a
//     stopped daemon has to cost nothing and print nothing — never an error.
//   - Colour degrades: truecolor gradient, then 256, then none, driven by the
//     environment rather than assumed.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
// provider has reported nothing yet.
func (w slWindow) headroom() float64 {
	if w.Limit <= 0 {
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

// pickAccount returns the one worth showing: whoever is serving, else the
// healthiest — that is the account the next request will land on.
func pickAccount(list []slAccount) *slAccount {
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
	// line itself, which is what the hook invokes.
	if len(args) > 0 {
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

	// Both endpoints, concurrently: the whole line has one 350ms budget, and
	// fetching them in series would double the worst case.
	var (
		list []slAccount
		st   slState
		aErr error
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		// A missing /api/state is not fatal — an older daemon still has
		// accounts to show, so the line degrades rather than disappearing.
		_ = getJSON(addr, token, "/api/state", &st)
	}()
	list, aErr = fetchAccounts(addr, token)
	<-done

	if aErr != nil || len(list) == 0 {
		return nil // daemon down or nothing configured: print nothing at all
	}
	fmt.Print(render(detectPalette(), list, st, time.Now()))
	return nil
}

// render builds the line. Split out from runStatusline so it can be tested
// without a daemon: this is the part with the judgement in it.
func render(p palette, list []slAccount, st slState, now time.Time) string {
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
		b.WriteString(degraded(p, st))
		return b.String()
	}

	a := pickAccount(list)
	if a == nil {
		// Nothing usable and nothing parked — `exhausted: fail` mode, or the
		// credentials are gone. Say when it lifts if we know.
		out := p.paint(gradeFor(0), "⛁ pool dry")
		if st.NextReset != nil {
			out += p.dim(" → " + st.NextReset.Local().Format("15:04") +
				" (" + compactDur(st.NextReset.Sub(now)) + ")")
		}
		return out + degraded(p, st)
	}

	var b strings.Builder
	b.WriteString(p.dim("⛁ "))
	b.WriteString(a.display())

	if a.LastModel != "" {
		b.WriteString(p.dim(" · "))
		b.WriteString(p.dim(shortModel(a.LastModel)))
	}

	for _, w := range a.Windows {
		h := w.headroom()
		if h < 0 {
			continue
		}
		b.WriteString("  ")
		b.WriteString(bar(p, h))
		b.WriteString(fmt.Sprintf(" %d%%", int(h*100+0.5)))
		b.WriteString(p.dim(" " + w.Name))
		// Once a window is nearly spent, "when does it come back" is the
		// question the percentage stops answering.
		if h <= 0.20 && !w.ResetAt.IsZero() {
			b.WriteString(p.dim(" ↻" + compactDur(w.ResetAt.Sub(now))))
		}
	}
	if len(a.Windows) == 0 {
		b.WriteString(p.dim("  no quota signal"))
	}
	if a.InFlight > 0 {
		b.WriteString(p.dim(fmt.Sprintf("  %d in flight", a.InFlight)))
	}
	b.WriteString(degraded(p, st))
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
func degraded(p palette, st slState) string {
	var b strings.Builder
	// Exhausted and Reserve differ only in how spillway found out — a 429
	// versus the account's own quota headers. From the prompt they are the
	// same fact: that account has nothing left.
	// Overage accounts are exhausted too, but they are reported separately
	// below — counting them here as well would double them.
	if n := st.Exhausted - st.Overage + st.Reserve; n > 0 {
		b.WriteString(p.dim(fmt.Sprintf("  %d spent", n)))
	}
	if st.Parked > 0 {
		b.WriteString("  " + p.paint(gradeFor(0.10), fmt.Sprintf("%d paused", st.Parked)))
	}
	if st.Disabled > 0 {
		b.WriteString("  " + p.paint(gradeFor(0), fmt.Sprintf("⚠ %d needs login", st.Disabled)))
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
func selfPath() (string, error) {
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
