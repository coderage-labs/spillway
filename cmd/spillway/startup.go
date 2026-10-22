package main

// Startup instrumentation and the bind-first scaffolding for issue #259.

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// processStart is when this Go program began running main's package init —
// the earliest moment it can observe about itself. Anything before it (the
// kernel exec, Gatekeeper scanning a freshly upgraded binary, I/O throttling
// of the loader) is invisible to the daemon's own clock, which is why
// logProcessStartGap asks the OS when the process really started.
var processStart = time.Now()

// startupPhases times each step of startup and logs it at INFO (issue #259:
// "measure, don't guess" the ~55s that preceded the old watchdog). Phases
// that finish before the logger exists (config, log open) are buffered and
// flushed by attach, with their true durations.
type startupPhases struct {
	mu      sync.Mutex
	logger  *slog.Logger
	last    time.Time
	pending []phaseRec
	current atomic.Value // string: the phase now running
}

type phaseRec struct {
	name    string
	elapsed time.Duration
	since   time.Duration
}

func newStartupPhases() *startupPhases {
	p := &startupPhases{last: time.Now()}
	return p
}

// begin names the phase now running, for the init watchdog's log line.
func (p *startupPhases) begin(name string) { p.current.Store(name) }

// Current is the phase that has begun and not yet been reported done.
func (p *startupPhases) Current() string {
	s, _ := p.current.Load().(string)
	return s
}

// done reports the phase that has just finished: its own elapsed time (since
// the previous done) and the total since process start.
func (p *startupPhases) done(name string) {
	p.mu.Lock()
	now := time.Now()
	rec := phaseRec{name: name, elapsed: now.Sub(p.last), since: now.Sub(processStart)}
	p.last = now
	l := p.logger
	if l == nil {
		p.pending = append(p.pending, rec)
	}
	p.mu.Unlock()
	if l != nil {
		logPhase(l, rec)
	}
}

// attach hands over the logger and flushes anything buffered before it
// existed.
func (p *startupPhases) attach(l *slog.Logger) {
	p.mu.Lock()
	p.logger = l
	pend := p.pending
	p.pending = nil
	p.mu.Unlock()
	for _, r := range pend {
		logPhase(l, r)
	}
}

func logPhase(l *slog.Logger, r phaseRec) {
	l.Info("startup phase", "phase", r.name,
		"elapsed", r.elapsed.Round(time.Millisecond),
		"sinceStart", r.since.Round(time.Millisecond))
}

// initWatchdogInterval is how often a daemon that is bound but not yet ready
// says so. Since #259 the listeners bind at once, so startupWatchdog (which
// watches the bind) can no longer notice a hang in the init that follows;
// this does, naming the phase that has not finished.
const initWatchdogInterval = 30 * time.Second

func initWatchdog(logger *slog.Logger, ready <-chan struct{}, ph *startupPhases, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ready:
			return
		case <-t.C:
			logger.Warn("spillway is bound but still initialising — requests are being held",
				"phase", ph.Current(), "sinceStart", time.Since(processStart).Round(time.Second))
		}
	}
}

// parsePsStart parses `ps -o lstart=` output ("Thu Oct 22 21:20:15 2026").
func parsePsStart(s string, loc *time.Location) (time.Time, error) {
	return time.ParseInLocation("Mon Jan _2 15:04:05 2006", strings.Join(strings.Fields(s), " "), loc)
}

// logProcessStartGap logs how long the process existed before main began —
// the part of a slow start the daemon's own timers cannot see. Best effort
// and POSIX-only (ps); a failure logs at debug and is otherwise silent.
func logProcessStartGap(logger *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-o", "lstart=", "-p", strconv.Itoa(os.Getpid()))
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		logger.Debug("could not read the OS process start time", "err", err)
		return
	}
	started, err := parsePsStart(string(out), time.Local)
	if err != nil {
		logger.Debug("could not parse the OS process start time", "out", strings.TrimSpace(string(out)), "err", err)
		return
	}
	// ps resolves to a second, so anything under 2s is rounding noise.
	gap := processStart.Sub(started)
	elapsed := gap.Round(time.Second).String()
	if gap < 2*time.Second {
		elapsed = "<2s"
	}
	logger.Info("startup phase", "phase", "before main (exec, loader, Gatekeeper, I/O throttling)",
		"elapsed", elapsed, "osProcessStart", started.Format(time.RFC3339))
}
