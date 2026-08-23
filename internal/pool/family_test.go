package pool

// Tests for issue #24: per-family quota. An account's fable bucket being
// spent must not read as the whole account being done, selection must prefer
// (never refuse for) an account with headroom in the family that governs the
// request, and stickiness breaks for a spent governing bucket by default but
// can be told to hold on instead.

import (
	"testing"
	"time"
)

func fableBody(model string) []byte {
	return []byte(`{"model":"` + model + `","max_tokens":16,"messages":[]}`)
}

const fableModel = "claude-opus-4-fable-preview"
const sonnetModel = "claude-sonnet-4-6"

func spentWindow(name string) []QuotaWindow {
	return []QuotaWindow{{Name: name, Limit: 1, Used: 1, Source: "headers", FetchedAt: time.Now()}}
}

// A fable request must prefer an account with fable headroom over one whose
// fable bucket is spent, even when the spent one is earlier in the pool's
// account list (so a pass here can't be explained by tie-break order alone).
func TestFableRequestPrefersAccountWithHeadroom(t *testing.T) {
	spent := NewAccount("spent", SourceYAML, "t", "", 0, "")
	spent.Type = "claude-oauth"
	spent.SetQuotaWindows(spentWindow("7d-fable"))

	clean := NewAccount("clean", SourceYAML, "t", "", 0, "")
	clean.Type = "claude-oauth"

	p := New([]*Account{spent, clean}, time.Now())
	got := p.SelectFor("s", fableBody(fableModel))
	if got == nil || got.Name != "clean" {
		t.Fatalf("SelectFor(fable) = %v, want %q (headroom)", gotName(got), "clean")
	}
}

// Deprioritise, not gate (#24 decision 1): when the fable-spent account is
// the only one, it must still serve rather than being refused outright.
func TestFableSpentAccountStillChosenWhenOnlyOption(t *testing.T) {
	spent := NewAccount("spent", SourceYAML, "t", "", 0, "")
	spent.Type = "claude-oauth"
	spent.SetQuotaWindows(spentWindow("7d-fable"))

	p := New([]*Account{spent}, time.Now())
	got := p.SelectFor("s", fableBody(fableModel))
	if got == nil {
		t.Fatal("SelectFor(fable) = nil, want the sole account served (deprioritise, not gate)")
	}
	if got.Name != "spent" {
		t.Fatalf("SelectFor(fable) = %q, want %q", got.Name, "spent")
	}
	if got.State() != StateOK {
		t.Errorf("over-threshold-for-family must not mark the account exhausted: state = %v", got.State())
	}
}

// A Sonnet request must be unaffected by a spent fable bucket: an account
// whose ONLY problem is fable is fully healthy for Sonnet.
//
// The "clean" competitor is deliberately given the WORSE priority. If fable
// wrongly gated Sonnet too, fableOnlySpent would fail tier 1 and clean would
// be the only tier-1 candidate left, winning despite its worse priority —
// so a pass here can't be explained by a tier-2 tie-break; it requires
// fableOnlySpent to have actually cleared tier 1 on merit.
func TestSonnetRequestUnaffectedBySpentFableBucket(t *testing.T) {
	fableOnlySpent := NewAccount("fable-only", SourceYAML, "t", "", 0, "")
	fableOnlySpent.Type = "claude-oauth"
	fableOnlySpent.SetPriority(0)
	fableOnlySpent.SetQuotaWindows(spentWindow("7d-fable"))

	clean := NewAccount("clean", SourceYAML, "t", "", 0, "")
	clean.Type = "claude-oauth"
	clean.SetPriority(1)

	p := New([]*Account{clean, fableOnlySpent}, time.Now())
	got := p.SelectFor("s", fableBody(sonnetModel))
	if got == nil || got.Name != "fable-only" {
		t.Fatalf("SelectFor(sonnet) = %v, want %q (fable spend doesn't apply to Sonnet, and it has the better priority)",
			gotName(got), "fable-only")
	}
}

// An unrecognised model must not be silently treated as fable: it has to
// resolve to the general windows, so a spent fable bucket must not gate it
// either. Same priority trick as the Sonnet test above, so a tier-2
// tie-break can't produce a false pass.
func TestUnknownModelGatesNothing(t *testing.T) {
	fableOnlySpent := NewAccount("fable-only", SourceYAML, "t", "", 0, "")
	fableOnlySpent.Type = "claude-oauth"
	fableOnlySpent.SetPriority(0)
	fableOnlySpent.SetQuotaWindows(spentWindow("7d-fable"))

	clean := NewAccount("clean", SourceYAML, "t", "", 0, "")
	clean.Type = "claude-oauth"
	clean.SetPriority(1)

	p := New([]*Account{clean, fableOnlySpent}, time.Now())
	got := p.SelectFor("s", fableBody("some-future-model-nobody-recognises"))
	if got == nil || got.Name != "fable-only" {
		t.Fatalf("SelectFor(unknown model) = %v, want %q (unknown must not gate on the fable bucket, and it has the better priority)",
			gotName(got), "fable-only")
	}
}

// Kimi has no family-scoped buckets: OverThresholdFor must behave exactly
// like the old, model-agnostic OverThreshold, whatever model is asked.
func TestKimiAccountsUnaffectedByFamilyLogic(t *testing.T) {
	k := NewAccount("kimi", SourceYAML, "t", "", 0, "")
	k.Type = "kimi-oauth"
	k.SetQuotaWindows(spentWindow("weekly"))

	for _, model := range []string{"", sonnetModel, fableModel, "anything-at-all"} {
		if got := k.OverThresholdFor(model, 0.98); got != k.OverThreshold(0.98) {
			t.Errorf("OverThresholdFor(%q) = %v, want it to match OverThreshold (%v): Kimi has no families",
				model, got, k.OverThreshold(0.98))
		}
		if !k.OverThresholdFor(model, 0.98) {
			t.Errorf("OverThresholdFor(%q) = false, want true: the weekly window is spent", model)
		}
	}
}

// Stickiness breaks by default when the sticky account's governing bucket
// for the request's family is spent, and a healthier account exists.
func TestStickinessBreaksOnFamilySpendByDefault(t *testing.T) {
	a := NewAccount("a", SourceYAML, "t", "", 0, "")
	a.Type = "claude-oauth"
	a.SetPriority(0)
	b := NewAccount("b", SourceYAML, "t", "", 0, "")
	b.Type = "claude-oauth"
	b.SetPriority(1)

	p := New([]*Account{a, b}, time.Now())

	first := p.SelectFor("s", fableBody(sonnetModel))
	if first == nil || first.Name != "a" {
		t.Fatalf("setup: first selection = %v, want %q (lower priority)", gotName(first), "a")
	}
	p.Done(first)

	a.SetQuotaWindows(spentWindow("7d-fable"))

	second := p.SelectFor("s", fableBody(fableModel))
	if second == nil || second.Name != "b" {
		t.Fatalf("SelectFor(fable) after A's fable bucket spent = %v, want %q (stickiness broken)",
			gotName(second), "b")
	}
}

// StickyAcrossFamily (#24 decision 2) restores the old behaviour: stay
// pinned to the sticky account even though its governing bucket for this
// request is spent, trading a possible refusal for the warm prompt cache.
func TestStickyAcrossFamilySettingKeepsTheSameAccount(t *testing.T) {
	a := NewAccount("a", SourceYAML, "t", "", 0, "")
	a.Type = "claude-oauth"
	a.SetPriority(0)
	b := NewAccount("b", SourceYAML, "t", "", 0, "")
	b.Type = "claude-oauth"
	b.SetPriority(1)

	p := New([]*Account{a, b}, time.Now())
	p.Apply(Settings{StickyAcrossFamily: true})

	first := p.SelectFor("s", fableBody(sonnetModel))
	if first == nil || first.Name != "a" {
		t.Fatalf("setup: first selection = %v, want %q", gotName(first), "a")
	}
	p.Done(first)

	a.SetQuotaWindows(spentWindow("7d-fable"))

	second := p.SelectFor("s", fableBody(fableModel))
	if second == nil || second.Name != "a" {
		t.Fatalf("SelectFor(fable) with StickyAcrossFamily = %v, want %q (stays pinned)",
			gotName(second), "a")
	}
}
