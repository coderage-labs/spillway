package main

import "testing"

// Issue #112: Anthropic reports utilization above 1.0 once an allowance is
// overshot, and `spillway status` rendered that as a negative percentage
// ("-8% left") while the dashboard and status line both clamped to 0%.
func TestHeadroomPctClampsOvershoot(t *testing.T) {
	cases := []struct {
		name        string
		used, limit float64
		want        int
	}{
		{"the reported bug: 109% utilisation", 1.09, 1.0, 0},
		{"far past the allowance", 3.0, 1.0, 0},
		{"exactly spent", 1.0, 1.0, 0},
		{"untouched", 0.0, 1.0, 100},
		{"half used", 0.5, 1.0, 50},
		{"rounds to nearest", 0.666, 1.0, 33},
		{"negative used cannot exceed full", -0.5, 1.0, 100},
		{"zero limit is unknown, not full", 0.5, 0, 0},
		{"negative limit is unknown", 0.5, -1, 0},
	}
	for _, c := range cases {
		if got := headroomPct(c.used, c.limit); got != c.want {
			t.Errorf("%s: headroomPct(%v, %v) = %d%%, want %d%%",
				c.name, c.used, c.limit, got, c.want)
		}
	}
}

// The three surfaces derive from one figure, so they must agree. Divergence
// is what made #112 read as the CLI reporting different data.
func TestHeadroomPctAgreesWithStatuslineClamp(t *testing.T) {
	for _, u := range []float64{0, 0.25, 0.5, 0.98, 1.0, 1.09, 2.5} {
		sl := slWindow{Used: u, Limit: 1.0}.headroom()
		want := int(sl*100 + 0.5)
		if got := headroomPct(u, 1.0); got != want {
			t.Errorf("used=%v: status %d%% but statusline %d%% — the two surfaces disagree", u, got, want)
		}
	}
}
