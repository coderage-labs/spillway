package main

// Issue #158, the status-line half.
//
// The prompt is the surface most likely to be on screen when someone wonders
// why only one of their accounts is being used, and it had two problems, not
// one: it never said a pin was set, and — because pickAccount ranked by
// headroom — it could name a completely different account from the one every
// next request was going to land on.

import (
	"strings"
	"testing"
	"time"
)

// pinPool: "spent" is pinned and nearly empty, "fresh" is untouched. Under
// automatic selection the line shows "fresh"; under a pin it must show
// "spent", because that is where the next request goes.
func pinPool(now time.Time) []slAccount {
	return []slAccount{
		{
			Name: "spent", Label: "spent", State: "ok",
			Windows: []slWindow{{Name: "5h", Limit: 100, Used: 95, ResetAt: now.Add(time.Hour)}},
		},
		{
			Name: "fresh", Label: "fresh", State: "ok",
			Windows: []slWindow{{Name: "5h", Limit: 100, Used: 5, ResetAt: now.Add(time.Hour)}},
		},
	}
}

// TestStatusLineMarksThePinnedAccount: the glyph, on the pinned account.
func TestStatusLineMarksThePinnedAccount(t *testing.T) {
	now := time.Now()
	out := render(noColour, pinPool(now), slState{Total: 2, Usable: 2, Pinned: "spent"}, now)

	if !strings.Contains(out, "📌") {
		t.Errorf("the pool row does not mark the pin:\n%s", out)
	}
	// Attached to the account, not floating at the end of the row: the pin
	// is a fact about WHICH account, so it has to be adjacent to the name.
	if !strings.Contains(out, "spent 📌") {
		t.Errorf("the pin marker is not next to the pinned account name:\n%s", out)
	}
}

// TestStatusLineShowsThePinnedAccountNotTheHealthiest is the behavioural
// half, and the reason the marker alone would not be enough: pickAccount
// used to rank by headroom, so a pin on a spent account made the prompt name
// the healthy one instead — an account that was not going to serve a single
// request.
func TestStatusLineShowsThePinnedAccountNotTheHealthiest(t *testing.T) {
	now := time.Now()
	list := pinPool(now)

	if got := pickAccount(list, ""); got == nil || got.Name != "fresh" {
		t.Fatalf("without a pin the healthiest account should be shown, got %v", got)
	}
	got := pickAccount(list, "spent")
	if got == nil {
		t.Fatal("pickAccount returned nothing while a pin was set")
	}
	if got.Name != "spent" {
		t.Errorf("a pin on %q showed %q instead — the prompt names an account that will serve nothing", "spent", got.Name)
	}
}

// A pin beats an in-flight request on some other account: that request is
// the tail of whatever preceded the pin, and every request after it goes to
// the pinned account.
func TestStatusLinePinBeatsAnInFlightRequestElsewhere(t *testing.T) {
	now := time.Now()
	list := pinPool(now)
	list[1].InFlight = 3 // "fresh" is finishing something

	got := pickAccount(list, "spent")
	if got == nil || got.Name != "spent" {
		t.Errorf("an in-flight request on another account displaced the pin, got %v", got)
	}
}

// A pin naming an account that is not in the list must not blank the row.
func TestStatusLinePinToAnUnknownAccountStillRenders(t *testing.T) {
	now := time.Now()
	out := render(noColour, pinPool(now), slState{Total: 2, Usable: 2, Pinned: "gone"}, now)

	if out == "" {
		t.Fatal("a pin naming no account emptied the pool row")
	}
	if !strings.Contains(out, "fresh") {
		t.Errorf("the row did not fall back to automatic selection:\n%s", out)
	}
	if strings.Contains(out, "📌") {
		t.Errorf("the row marked an account that is not the pinned one:\n%s", out)
	}
}

// TestStatusLineUnpinnedIsUnchanged: the unpinned pool row, byte for byte,
// exactly as it rendered before #158. Asserted as a whole string rather than
// by searching for the glyph, because a marker that renders as a stray space
// — or a conditional that inserts one — would pass a Contains check and
// still shift every column on the row.
//
// st.Pinned is left at "", which is what an automatic daemon actually serves:
// /api/state's pinned is omitempty, so it is absent from the JSON and decodes
// to the zero value.
func TestStatusLineUnpinnedIsUnchanged(t *testing.T) {
	now := time.Now()
	list, st := demoPool(now)
	sess := demoSession(t)

	const wantUntrimmed = "\u26c1 csno  \U0001f3af fable-5-1  \u23f3 \u2588\u2588\u2588\u2588\u2588\u2588\u2588\u2591 91% 5h" +
		"  \U0001f4c6 \u2588\u2591\u2591\u2591\u2591\u2591\u2591\u2591 11% 7d \u21bb5d1h" +
		"  \U0001f4c6 \u2588\u2588\u2588\u2591\u2591\u2591\u2591\u2591 40% 7d-fable  \u2708 2  \U0001f480 6"
	_, got := rowsOf(t, renderLine(noColour, list, st, sess, now, 0))
	if got != wantUntrimmed {
		t.Errorf("the unpinned pool row changed:\n--- got  ---\n%q\n--- want ---\n%q", got, wantUntrimmed)
	}

	for _, cols := range []int{0, 120, 80, 60} {
		line := renderLine(noColour, list, st, sess, now, cols)
		if strings.Contains(line, "📌") {
			t.Errorf("at %d columns an unpinned pool grew a pin marker:\n%s", cols, line)
		}
	}
}

// TestPinnedRowTrimsInTheAgreedOrder: the pin marker is not in poolTrim's
// order, so it must survive every reduction — and the reductions themselves
// must still run 💀 → ✈ → fable, at the widths where each becomes necessary
// once the marker's three columns are accounted for.
func TestPinnedRowTrimsInTheAgreedOrder(t *testing.T) {
	now := time.Now()
	list, st := demoPool(now)
	st.Pinned = list[0].Name // "csno@example.com"
	sess := demoSession(t)

	_, full := rowsOf(t, renderLine(noColour, list, st, sess, now, 0))
	if !strings.Contains(full, "csno 📌") {
		t.Fatalf("the untrimmed pinned row does not mark the pin: %q", full)
	}

	// Three columns wider than the unpinned row, so each step lands three
	// columns later: 100, 95 and 69 against #183's 97, 92 and 66.
	at100, _ := poolRowAt(t, list, st, sess, now, 100)
	if strings.Contains(at100, "💀") {
		t.Errorf("at 100 columns the pinned row %q kept the spent count, which is first to go", at100)
	}
	if !strings.Contains(at100, "✈ 2") || !strings.Contains(at100, "7d-fable") {
		t.Errorf("at 100 columns the pinned row %q dropped in-flight or the fable window before spent", at100)
	}

	at95, _ := poolRowAt(t, list, st, sess, now, 95)
	if strings.Contains(at95, "✈") {
		t.Errorf("at 95 columns the pinned row %q kept the in-flight count", at95)
	}
	if !strings.Contains(at95, "7d-fable") {
		t.Errorf("at 95 columns the pinned row %q dropped the fable window before in-flight", at95)
	}

	at69, _ := poolRowAt(t, list, st, sess, now, 69)
	if strings.Contains(at69, "7d-fable") {
		t.Errorf("at 69 columns the pinned row %q kept the fable window", at69)
	}

	// The marker outlives all of them, including the width at which #183's
	// last documented reduction has already fired.
	for _, row := range []string{at100, at95, at69} {
		if !strings.Contains(row, "📌") {
			t.Errorf("a trim dropped the pin marker: %q", row)
		}
	}
}

// And at 80 columns — the width #183 was fought over — the pinned pool row
// is this, exactly.
//
// A whole-string assertion rather than a fit check: fitPool's last resort is
// truncateToWidth, so "it fits" is true of any row by construction and
// proves nothing. What has to hold is that the row fits by TRIMMING in the
// agreed order (spent, then in-flight, then fable) and not by being cut —
// and that the pin marker, which is in no trim level, is still there when
// the reductions have finished.
func TestPinnedLineFitsEightyColumns(t *testing.T) {
	now := time.Now()
	list, st := demoPool(now)
	st.Pinned = list[0].Name
	sess := demoSession(t)

	const want = "\u26c1 csno \U0001f4cc  \U0001f3af fable-5-1  \u23f3 \u2588\u2588\u2588\u2588\u2588\u2588\u2588\u2591 91% 5h" +
		"  \U0001f4c6 \u2588\u2591\u2591\u2591\u2591\u2591\u2591\u2591 11% 7d \u21bb5d1h"

	line := renderLine(noColour, list, st, sess, now, 80)
	rows := strings.Split(line, "\n")
	if len(rows) != 2 {
		t.Fatalf("expected two rows, got %d: %q", len(rows), line)
	}
	if rows[1] != want {
		t.Errorf("the pinned pool row at 80 columns changed:\n--- got  ---\n%q\n--- want ---\n%q", rows[1], want)
	}
	if w := displayWidth(rows[1]); w > 80 {
		t.Errorf("pinned pool row is %d columns wide and will wrap: %q", w, rows[1])
	}
}
