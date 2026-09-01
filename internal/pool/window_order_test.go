package pool

import (
	"sort"
	"testing"
)

// The reported order: tanks appeared in arrival order, so one account showed
// [7d-fable 7d 5h] while another showed [7d-fable 5h 7d] on the same page.
func TestWindowOrderIsShortestFirstThenVariant(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "the reported case",
			in:   []string{"7d-fable", "7d", "5h"},
			want: []string{"5h", "7d", "7d-fable"},
		},
		{
			name: "the other account, same page",
			in:   []string{"7d-fable", "5h", "7d"},
			want: []string{"5h", "7d", "7d-fable"},
		},
		{
			// Lexically "24h" < "5h" and "5h" > "7d", so a name sort gets
			// both of these wrong. This is why the rank parses a duration.
			name: "durations, not strings",
			in:   []string{"7d", "24h", "5h", "30d"},
			want: []string{"5h", "24h", "7d", "30d"},
		},
		{
			name: "a variant sorts immediately after its base",
			in:   []string{"5h-preview", "7d", "5h"},
			want: []string{"5h", "5h-preview", "7d"},
		},
		{
			// Kimi's "weekly" has no parseable duration. It must not be
			// guessed at, and must not destabilise the windows that do.
			name: "unparseable names go last, stably",
			in:   []string{"weekly", "7d", "5h", "parallel"},
			want: []string{"5h", "7d", "parallel", "weekly"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := append([]string(nil), c.in...)
			sort.Slice(got, func(i, j int) bool { return lessWindow(got[i], got[j]) })
			for i := range c.want {
				if got[i] != c.want[i] {
					t.Fatalf("got %v, want %v", got, c.want)
				}
			}
		})
	}
}

// Whatever order the windows were stored in, the accessor every display site
// reads through must hand them back sorted.
func TestQuotaWindowsAccessorReturnsSorted(t *testing.T) {
	a := NewAccount("you@acme.example", SourceYAML, "t", "", 0, "")
	a.SetQuotaWindows([]QuotaWindow{
		{Name: "7d-fable", Limit: 1, Used: 0.5},
		{Name: "7d", Limit: 1, Used: 0.5},
		{Name: "5h", Limit: 1, Used: 0.5},
	})
	got := a.QuotaWindows()
	want := []string{"5h", "7d", "7d-fable"}
	for i, w := range want {
		if got[i].Name != w {
			names := make([]string, len(got))
			for j := range got {
				names[j] = got[j].Name
			}
			t.Fatalf("QuotaWindows() = %v, want %v", names, want)
		}
	}
}
