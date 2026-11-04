package pool

import (
	"encoding/json"
	"testing"
	"time"
)

const summaryA = compactionMarker + " The summary below covers the earlier portion of the conversation.\n\nSummary A"
const summaryB = compactionMarker + " The summary below covers the earlier portion of the conversation.\n\nSummary B"

func coldBody(t *testing.T, first any, ttl string, extra ...any) []byte {
	t.Helper()
	cc := map[string]any{"type": "ephemeral"}
	if ttl != "" {
		cc["ttl"] = ttl
	}
	msgs := []any{map[string]any{"role": "user", "content": first}}
	msgs = append(msgs, extra...)
	b, err := json.Marshal(map[string]any{
		"model":    "claude-sonnet-4-5",
		"system":   []any{map[string]any{"type": "text", "text": "sys", "cache_control": cc}},
		"messages": msgs,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func summaryBlocks(s string) any {
	return []any{map[string]any{"type": "text", "text": s}}
}

// coldPool: "low" is sticky for key k though "top" outranks it.
func coldPool(t *testing.T) (*Pool, *Account, *Account) {
	t.Helper()
	top := NewAccount("top", SourceYAML, "t", "", 0, "")
	top.Type = "claude-oauth"
	low := NewAccount("low", SourceYAML, "t", "", 0, "")
	low.Type = "claude-oauth"
	low.priority = 6
	p := New([]*Account{top, low}, time.Now())
	p.sticky["k"] = "low"
	return p, top, low
}

// step runs one request the way the proxy does and returns the chosen name
// and whether a cold unpin fired.
func step(t *testing.T, p *Pool, body []byte, now time.Time) (string, ColdUnpin, bool) {
	t.Helper()
	u, moved := p.ReconsiderSticky("k", ParseCacheSignals(body), now)
	a := p.SelectExcept("k", body, nil)
	if a == nil {
		t.Fatal("nothing selected")
	}
	p.Done(a)
	return a.Name, u, moved
}

func TestColdCompactionMovesToTop(t *testing.T) {
	p, _, _ := coldPool(t)
	now := time.Now()
	name, u, moved := step(t, p, coldBody(t, summaryBlocks(summaryA), "1h"), now)
	if name != "top" || !moved || u.Trigger != "compaction" || u.From != "low" {
		t.Fatalf("got %s %+v moved=%v", name, u, moved)
	}
}

func TestColdCompactionStringContent(t *testing.T) {
	p, _, _ := coldPool(t)
	name, _, moved := step(t, p, coldBody(t, summaryA, "1h"), time.Now())
	if name != "top" || !moved {
		t.Fatalf("string content: got %s moved=%v", name, moved)
	}
}

// The summarisation request itself reads the whole conversation from cache:
// messages[0] is the original first message and the last message is the
// summarisation instruction. It must not move.
func TestColdCompactionRequestItselfStays(t *testing.T) {
	p, _, _ := coldPool(t)
	instr := map[string]any{"role": "user", "content": "Your task is to create a detailed summary of the conversation so far."}
	body := coldBody(t, summaryBlocks("Fix the login bug in auth.go"), "1h",
		map[string]any{"role": "assistant", "content": "ok"}, instr)
	name, _, moved := step(t, p, body, time.Now())
	if name != "low" || moved {
		t.Fatalf("compaction request moved: %s moved=%v", name, moved)
	}
}

// A second compaction's summarisation request has the PREVIOUS summary as
// messages[0] (already seen) and must not move either.
func TestColdSecondCompactionRequestItselfStays(t *testing.T) {
	p, _, _ := coldPool(t)
	now := time.Now()
	step(t, p, coldBody(t, summaryBlocks(summaryA), "1h"), now) // first post-compaction
	p.sticky["k"] = "low"                                       // park it back on low
	instr := map[string]any{"role": "user", "content": "Your task is to create a detailed summary."}
	body := coldBody(t, summaryBlocks(summaryA), "1h", map[string]any{"role": "assistant", "content": "x"}, instr)
	name, _, moved := step(t, p, body, now.Add(time.Second))
	if name != "low" || moved {
		t.Fatalf("got %s moved=%v", name, moved)
	}
}

func TestColdSameSummaryDoesNotRetrigger(t *testing.T) {
	p, _, _ := coldPool(t)
	now := time.Now()
	body := coldBody(t, summaryBlocks(summaryA), "1h")
	step(t, p, body, now)
	p.sticky["k"] = "low"
	name, _, moved := step(t, p, body, now.Add(time.Second))
	if name != "low" || moved {
		t.Fatalf("retriggered: %s moved=%v", name, moved)
	}
}

func TestColdSecondCompactionTriggers(t *testing.T) {
	p, _, _ := coldPool(t)
	now := time.Now()
	step(t, p, coldBody(t, summaryBlocks(summaryA), "1h"), now)
	p.sticky["k"] = "low"
	name, u, moved := step(t, p, coldBody(t, summaryBlocks(summaryB), "1h"), now.Add(time.Second))
	if name != "top" || !moved || u.Trigger != "compaction" {
		t.Fatalf("got %s %+v moved=%v", name, u, moved)
	}
}

func TestColdIdleOneHourTTL(t *testing.T) {
	plain := coldBody(t, "hello", "1h")
	now := time.Now()

	p, _, _ := coldPool(t)
	step(t, p, plain, now) // records last use (sticky stays: no marker, no idle yet)
	name, _, moved := step(t, p, plain, now.Add(59*time.Minute))
	if name != "low" || moved {
		t.Fatalf("under 1h moved: %s", name)
	}
	name, u, moved := step(t, p, plain, now.Add(59*time.Minute+61*time.Minute))
	if name != "top" || !moved || u.Trigger != "idle" || u.From != "low" {
		t.Fatalf("past 1h: %s %+v moved=%v", name, u, moved)
	}
}

func TestColdFiveMinuteTTLUsesFive(t *testing.T) {
	body := coldBody(t, "hello", "") // ephemeral, no ttl => 5m
	if got := ParseCacheSignals(body).TTL; got != 5*time.Minute {
		t.Fatalf("ttl = %v", got)
	}
	p, _, _ := coldPool(t)
	now := time.Now()
	step(t, p, body, now)
	name, _, moved := step(t, p, body, now.Add(6*time.Minute))
	if name != "top" || !moved {
		t.Fatalf("6m idle on 5m TTL: %s moved=%v", name, moved)
	}
}

func TestColdMaxTTLAcrossSystemToolsMessages(t *testing.T) {
	b, _ := json.Marshal(map[string]any{
		"system": "plain string system",
		"tools":  []any{map[string]any{"name": "t", "cache_control": map[string]any{"type": "ephemeral", "ttl": "1h"}}},
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "x", "cache_control": map[string]any{"type": "ephemeral"}}}}},
	})
	if got := ParseCacheSignals(b).TTL; got != time.Hour {
		t.Fatalf("tools 1h ignored: %v", got)
	}
	b, _ = json.Marshal(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "x", "cache_control": map[string]any{"type": "ephemeral", "ttl": "1h"}}}}},
	})
	if got := ParseCacheSignals(b).TTL; got != time.Hour {
		t.Fatalf("message 1h ignored: %v", got)
	}
	b, _ = json.Marshal(map[string]any{"system": []any{map[string]any{"type": "text", "text": "s", "cache_control": map[string]any{"ttl": "2h"}}}})
	if got := ParseCacheSignals(b).TTL; got != 5*time.Minute {
		t.Fatalf("other ttl should be 5m: %v", got)
	}
}

func TestColdPinnedUnaffected(t *testing.T) {
	p, _, _ := coldPool(t)
	if err := p.Pin("low", true); err != nil {
		t.Fatal(err)
	}
	name, _, moved := step(t, p, coldBody(t, summaryBlocks(summaryA), "1h"), time.Now())
	if name != "low" || moved {
		t.Fatalf("pin overridden: %s moved=%v", name, moved)
	}
	if p.sticky["k"] != "low" {
		t.Fatal("sticky entry deleted under a pin")
	}
}

func TestColdNoMarkerRecentStaysSticky(t *testing.T) {
	p, _, _ := coldPool(t)
	now := time.Now()
	body := coldBody(t, "hello", "1h")
	step(t, p, body, now)
	name, _, moved := step(t, p, body, now.Add(time.Minute))
	if name != "low" || moved {
		t.Fatalf("moved: %s", name)
	}
}

func TestColdUnpinKeepsCurrentWhenStillBest(t *testing.T) {
	p, _, _ := coldPool(t)
	p.sticky["k"] = "top"
	name, _, moved := step(t, p, coldBody(t, summaryBlocks(summaryA), "1h"), time.Now())
	if name != "top" || !moved {
		t.Fatalf("got %s moved=%v", name, moved)
	}
}

func TestColdStateBounded(t *testing.T) {
	p, _, _ := coldPool(t)
	now := time.Now()
	for i := 0; i < maxColdKeys*3; i++ {
		p.ReconsiderSticky("key-"+string(rune(i))+time.Duration(i).String(), CacheSignals{TTL: time.Hour}, now.Add(time.Duration(i)*time.Millisecond))
	}
	if n := len(p.cold); n > maxColdKeys {
		t.Fatalf("cold map grew to %d", n)
	}
	// Stale entries are swept before live ones are touched.
	p2, _, _ := coldPool(t)
	for i := 0; i < maxColdKeys; i++ {
		p2.ReconsiderSticky("old-"+time.Duration(i).String(), CacheSignals{TTL: time.Hour}, now)
	}
	p2.ReconsiderSticky("new", CacheSignals{TTL: time.Hour}, now.Add(2*time.Hour))
	if len(p2.cold) != 1 {
		t.Fatalf("stale entries not swept: %d", len(p2.cold))
	}
}

func TestColdMalformedBodyFailsSafe(t *testing.T) {
	for _, b := range [][]byte{nil, []byte("{"), []byte(`{"messages":[{"role":"user","content":5}]}`), []byte(`{"messages":[]}`)} {
		if s := ParseCacheSignals(b); s.Compaction || s.TTL != 5*time.Minute {
			t.Fatalf("%s: %+v", b, s)
		}
	}
}

// A short-TTL request on a key that has asked for 1h (Claude Code's background
// Haiku calls cache for the default 5m) must not unpin a main thread whose 1h
// cache is still warm.
func TestColdShortTTLRequestDoesNotUnpinLongTTLKey(t *testing.T) {
	long := coldBody(t, "hello", "1h")
	short := coldBody(t, "title this", "")
	now := time.Now()

	p, _, _ := coldPool(t)
	step(t, p, long, now)
	name, _, moved := step(t, p, short, now.Add(10*time.Minute))
	if name != "low" || moved {
		t.Fatalf("5m-TTL request after 10m unpinned a 1h-TTL key: %s moved=%v", name, moved)
	}
	name, u, moved := step(t, p, long, now.Add(10*time.Minute+61*time.Minute))
	if name != "top" || !moved || u.Trigger != "idle" {
		t.Fatalf("past the key's 1h TTL: %s %+v moved=%v", name, u, moved)
	}
}
