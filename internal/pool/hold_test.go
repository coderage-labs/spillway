package pool

import (
	"sync"
	"testing"
	"time"
)

func TestHoldsReportsCountAndEarliest(t *testing.T) {
	p := New(nil, time.Now())
	if n, until := p.Holds(); n != 0 || !until.IsZero() {
		t.Fatalf("fresh pool reports %d holds until %v", n, until)
	}

	later := time.Now().Add(2 * time.Hour)
	sooner := time.Now().Add(30 * time.Minute)
	relLater := p.BeginHold(later)
	relSooner := p.BeginHold(sooner)

	n, until := p.Holds()
	if n != 2 {
		t.Errorf("holds = %d, want 2", n)
	}
	// The earliest is what the status line shows: it is when the user gets
	// their session back, not when the last one clears.
	if !until.Equal(sooner) {
		t.Errorf("until = %v, want the soonest hold %v", until, sooner)
	}

	relSooner()
	if n, until := p.Holds(); n != 1 || !until.Equal(later) {
		t.Errorf("after release: %d holds until %v, want 1 until %v", n, until, later)
	}
	relLater()
	if n, _ := p.Holds(); n != 0 {
		t.Errorf("all released but %d holds remain", n)
	}
}

// A deferred release on a path that already released must not corrupt the
// count — it would otherwise be easy to double-release and go negative.
func TestReleaseIsIdempotent(t *testing.T) {
	p := New(nil, time.Now())
	rel := p.BeginHold(time.Now().Add(time.Hour))
	other := p.BeginHold(time.Now().Add(time.Hour))
	rel()
	rel()
	rel()
	if n, _ := p.Holds(); n != 1 {
		t.Errorf("holds = %d after repeated release, want the 1 still parked", n)
	}
	other()
}

func TestHoldsIsRaceFree(t *testing.T) {
	p := New(nil, time.Now())
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			rel := p.BeginHold(time.Now().Add(time.Hour))
			rel()
		}()
		go func() {
			defer wg.Done()
			p.Holds()
		}()
	}
	wg.Wait()
	if n, _ := p.Holds(); n != 0 {
		t.Errorf("holds = %d after all released", n)
	}
}
