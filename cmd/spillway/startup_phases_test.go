package main

import (
	"strings"
	"testing"
	"time"
)

func TestStartupPhasesBufferThenFlushWithDurations(t *testing.T) {
	ph := newStartupPhases()
	ph.begin("config")
	time.Sleep(5 * time.Millisecond)
	ph.done("config") // before any logger exists
	l, buf := bufLogger()
	ph.attach(l)
	ph.begin("bind")
	ph.done("bind")
	out := buf.String()
	for _, want := range []string{"phase=config", "phase=bind", "elapsed=", "sinceStart="} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "phase=config") > strings.Index(out, "phase=bind") {
		t.Error("buffered phase logged after a later one")
	}
	if ph.Current() != "bind" {
		t.Errorf("Current = %q", ph.Current())
	}
}

func TestInitWatchdogNamesThePhaseUntilReady(t *testing.T) {
	l, buf := bufLogger()
	ph := newStartupPhases()
	ph.begin("pool")
	ready := make(chan struct{})
	done := make(chan struct{})
	go func() { initWatchdog(l, ready, ph, 10*time.Millisecond); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(buf.String(), "phase=pool") {
		if time.Now().After(deadline) {
			t.Fatalf("watchdog never named the phase: %q", buf.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(ready)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog did not stop on ready")
	}
}

func TestParsePsStart(t *testing.T) {
	got, err := parsePsStart("Thu Oct  2 21:20:15 2026\n", time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 10, 2, 21, 20, 15, 0, time.UTC); !got.Equal(want) {
		t.Errorf("got %v want %v", got, want)
	}
}
