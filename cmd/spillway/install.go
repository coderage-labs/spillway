package main

// `spillway install` sets up everything spillway wants on this machine, so
// that a new user runs one command rather than three they have to know about.
//
// It is deliberately a registry of steps over the existing subcommands, not a
// second implementation of them. Each step stays independently runnable —
// `spillway service install` still works and is still the thing to reach for
// when only one part is wrong — and adding another (a hook, say) means adding
// one entry here rather than touching the composite's logic.
//
// No step aborts the others. A machine can legitimately want the daemon and
// not the plugin, and a partial success reported honestly is more useful than
// an install that stops at the first thing it cannot do.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// DefaultPluginSource is the marketplace `spillway install` registers. A
// clone that wants its own working copy instead passes --plugin-source.
const DefaultPluginSource = "coderage-labs/spillway"

// step is one installable piece.
type step struct {
	name string
	// unavailable explains why this step cannot run here, or "" if it can.
	// Not an error: a box with no `claude` on it is a fine place to run the
	// daemon, and saying so beats failing the whole install.
	unavailable func() string
	install     func(opts installOpts) error
	uninstall   func() error
	status      func() error
}

type installOpts struct {
	pluginSource string
	force        bool
}

// installSteps is a variable so tests can drive the orchestration without
// touching launchd or the user's Claude Code config.
var installSteps = defaultSteps

func defaultSteps() []step {
	return []step{
		{
			name:      "service",
			install:   func(installOpts) error { return serviceInstall() },
			uninstall: serviceUninstall,
			status:    serviceStatus,
		},
		{
			name:        "status line",
			unavailable: needClaudeSettings,
			install: func(o installOpts) error {
				var args []string
				if o.force {
					args = append(args, "--force")
				}
				return runStatuslineInstall(args)
			},
			uninstall: runStatuslineUninstall,
			status:    runStatuslineStatus,
		},
		{
			name:        "plugin",
			unavailable: needClaudeCLI,
			install:     pluginInstall,
			uninstall:   pluginUninstall,
			status:      pluginStatus,
		},
	}
}

func runInstall(args []string) error {
	var o installOpts
	o.pluginSource = DefaultPluginSource
	action := "install"
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--force" || a == "-f":
			o.force = true
		case a == "--plugin-source":
			if i+1 >= len(args) {
				return errors.New("--plugin-source needs a path or owner/repo")
			}
			i++
			o.pluginSource = args[i]
		case strings.HasPrefix(a, "--plugin-source="):
			o.pluginSource = strings.TrimPrefix(a, "--plugin-source=")
		case strings.HasPrefix(a, "-"):
			return fmt.Errorf("unknown flag %q", a)
		default:
			action = a
		}
	}

	// Say which binary is being wired up, before anything is written rather
	// than after. Both the launchd job and the status line record an absolute
	// path, so running this from a scratch build points the machine at a
	// binary that will not be there tomorrow — and the only clue was buried
	// in a step's own output further down.
	if action == "install" {
		if bin, err := selfPath(); err == nil {
			fmt.Printf("wiring up %s\n", bin)
		}
	}

	var failed []string
	for _, s := range installSteps() {
		fmt.Printf("\n== %s ==\n", s.name)
		if s.unavailable != nil {
			if why := s.unavailable(); why != "" {
				fmt.Printf("skipped: %s\n", why)
				continue
			}
		}
		var err error
		switch action {
		case "install":
			err = s.install(o)
		case "uninstall":
			err = s.uninstall()
		case "status":
			err = s.status()
		default:
			return fmt.Errorf("unknown action %q (install|uninstall|status)", action)
		}
		if err != nil {
			fmt.Printf("failed: %v\n", err)
			failed = append(failed, s.name)
		}
	}
	fmt.Println()
	if len(failed) > 0 {
		return fmt.Errorf("%s incomplete: %s", action, strings.Join(failed, ", "))
	}
	return nil
}

// needClaudeCLI reports why the plugin steps cannot run, or "".
func needClaudeCLI() string {
	if _, err := exec.LookPath("claude"); err != nil {
		return "`claude` is not on PATH, so there is nothing to install a plugin into"
	}
	return ""
}

// needClaudeSettings reports why the status line cannot be wired up. The
// status line is written into Claude Code's settings, so the same absence
// that rules out the plugin rules this out too.
func needClaudeSettings() string {
	return needClaudeCLI()
}

// claudeCmd runs one `claude plugin ...` invocation, passing its output
// through. The plugin registry is Claude Code's own state across every
// plugin the user has, spread over several files it versions itself, so it
// is written with its CLI rather than by editing that state behind its back.
func claudeCmd(args ...string) error {
	cmd := exec.Command("claude", args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// marketplaceKnown reports whether a marketplace named spillway is already
// registered. Used only to decide whether to skip the add, so a wrong answer
// costs an idempotent no-op, never a wrong source.
func marketplaceKnown() bool {
	out, err := exec.Command("claude", "plugin", "marketplace", "list").Output()
	if err != nil {
		return false
	}
	return hasSpillwayMarketplace(string(out))
}

// hasSpillwayMarketplace finds spillway's entry in `claude plugin marketplace
// list` output. Matching the whole name matters: "spillway" and a
// hypothetical "spillway-extras" must not be confused for one another.
func hasSpillwayMarketplace(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "❯")) == "spillway" {
			return true
		}
	}
	return false
}

func pluginInstall(o installOpts) error {
	// Never re-point an existing marketplace. A clone registered as a
	// directory source is someone working on spillway, and silently
	// replacing it with the released copy would break their edit loop with
	// no way to tell from the output that it had happened.
	if marketplaceKnown() {
		fmt.Println("marketplace 'spillway' already registered — leaving its source alone")
	} else if err := claudeCmd("plugin", "marketplace", "add", o.pluginSource); err != nil {
		return fmt.Errorf("add marketplace %s: %w", o.pluginSource, err)
	}
	return claudeCmd("plugin", "install", "spillway@spillway")
}

func pluginUninstall() error {
	if err := claudeCmd("plugin", "uninstall", "spillway@spillway"); err != nil {
		return err
	}
	return claudeCmd("plugin", "marketplace", "remove", "spillway")
}

// pluginStatus reports only spillway's entry. `claude plugin list` prints
// every plugin the user has, which in a composite status is noise about other
// people's software.
func pluginStatus() error {
	if !marketplaceKnown() {
		fmt.Println("not installed: no 'spillway' marketplace registered")
		return nil
	}
	out, err := exec.Command("claude", "plugin", "list").Output()
	if err != nil {
		return err
	}
	block := spillwayPluginBlock(string(out))
	if len(block) == 0 {
		fmt.Println("marketplace registered, plugin not installed")
		return nil
	}
	fmt.Println(strings.Join(block, "\n"))
	return nil
}

// spillwayPluginBlock pulls spillway's stanza out of `claude plugin list`,
// which prints one indented block per plugin separated by blank lines.
func spillwayPluginBlock(out string) []string {
	var block []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "spillway@spillway") {
			block = append(block, line)
			continue
		}
		if len(block) > 0 {
			if strings.TrimSpace(line) == "" {
				break
			}
			block = append(block, line)
		}
	}
	return block
}
