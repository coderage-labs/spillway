package pool

// Cold-cache re-routing (issue #267).
//
// Sticky routing keeps a conversation on one account so its prompt cache
// stays warm, but it also keeps it there long after a better-priority account
// has headroom. Two moments make the cache cold anyway, so moving costs only
// the system+tools re-cache:
//
//   - the first request after a compaction (the transcript is replaced by a
//     summary, so everything past system+tools is new);
//   - a request arriving after the sticky entry has been idle longer than the
//     prompt-cache TTL.
//
// In both cases the decision only DELETES the sticky entry; normal selection
// (priority, then load) then runs, so the current account stays if it is
// still the best. A pin wins before sticky and is left alone.

import (
	"crypto/sha256"
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// compactionMarker is the first sentence of the fixed first user message
// Claude Code starts a compacted conversation with (verified against real
// transcripts; usually followed by " The summary below covers the earlier
// portion of the conversation."). Only the first sentence is matched, so
// wording drift after it is harmless, and a reword of the sentence itself
// fails safe: nothing fires and behaviour is as before.
const compactionMarker = "This session is being continued from a previous conversation that ran out of context."

const (
	defaultCacheTTL = 5 * time.Minute
	longCacheTTL    = time.Hour

	// maxColdKeys bounds the per-key state. It is a safety valve: Claude
	// Code opens one key per CLI session, so this is far above any real
	// working set.
	maxColdKeys = 4096
)

// CacheSignals is what one request body says about its prompt cache.
type CacheSignals struct {
	// Compaction: messages[0] carries the compaction marker.
	Compaction bool
	// SummaryHash identifies that summary (valid when Compaction).
	SummaryHash [32]byte
	// TTL is the longest cache_control ttl any block asks for; 5m default.
	TTL time.Duration
}

type cacheBlock struct {
	Type         string `json:"type"`
	Text         string `json:"text"`
	CacheControl *struct {
		TTL string `json:"ttl"`
	} `json:"cache_control"`
}

// blocks decodes a content value that is a string, an array of blocks, or
// anything else (ignored). A bare string becomes one text block.
func blocks(raw json.RawMessage) []cacheBlock {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 {
		return nil
	}
	switch raw[0] {
	case '"':
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return nil
		}
		return []cacheBlock{{Type: "text", Text: s}}
	case '[':
		var bs []cacheBlock
		if json.Unmarshal(raw, &bs) != nil {
			return nil
		}
		return bs
	}
	return nil
}

// ParseCacheSignals reads a request body once and returns its cache
// signals. Unparseable bodies give the zero signal with the default TTL.
func ParseCacheSignals(body []byte) CacheSignals {
	sig := CacheSignals{TTL: defaultCacheTTL}
	if len(body) == 0 {
		return sig
	}
	var v struct {
		System   json.RawMessage   `json:"system"`
		Tools    []cacheBlock      `json:"tools"`
		Messages []json.RawMessage `json:"messages"`
	}
	if json.Unmarshal(body, &v) != nil {
		return sig
	}
	scan := func(bs []cacheBlock) {
		for _, b := range bs {
			if b.CacheControl != nil && b.CacheControl.TTL == "1h" {
				sig.TTL = longCacheTTL
			}
		}
	}
	scan(blocks(v.System))
	scan(v.Tools)
	for i, m := range v.Messages {
		var msg struct {
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(m, &msg) != nil {
			continue
		}
		bs := blocks(msg.Content)
		scan(bs)
		if i == 0 && len(bs) > 0 && strings.HasPrefix(bs[0].Text, compactionMarker) {
			sig.Compaction = true
			sig.SummaryHash = sha256.Sum256([]byte(bs[0].Text))
		}
	}
	return sig
}

// coldState is the per-routing-key memory for the two triggers.
type coldState struct {
	summary [32]byte // last compaction summary seen (zero = none)
	lastUse time.Time
}

// ColdUnpin describes a sticky entry ReconsiderSticky deleted.
type ColdUnpin struct {
	Trigger string // "compaction" or "idle"
	From    string // account the key was sticky to
}

// ReconsiderSticky applies the cold-cache rules for one request, before the
// first selection. session is the same key SelectExcept's sticky map uses.
// It records the request (last use, last summary) whatever it decides, and
// returns the unpin it performed, if any. Call it once per request, not once
// per retry.
//
// Fail-safe by construction: a key with no state (first sight, restart,
// evicted) never fires the idle trigger, and a body without the marker never
// fires the compaction one.
func (p *Pool) ReconsiderSticky(session string, sig CacheSignals, now time.Time) (ColdUnpin, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	st := p.cold[session]
	seen := st != nil
	if !seen {
		if p.cold == nil {
			p.cold = map[string]*coldState{}
		}
		p.evictColdLocked(now)
		st = &coldState{}
		p.cold[session] = st
	}
	prevUse := st.lastUse
	st.lastUse = now

	trigger := ""
	if sig.Compaction && sig.SummaryHash != st.summary {
		st.summary = sig.SummaryHash
		trigger = "compaction"
	} else if seen && !prevUse.IsZero() && sig.TTL > 0 && now.Sub(prevUse) > sig.TTL {
		trigger = "idle"
	}
	if trigger == "" {
		return ColdUnpin{}, false
	}
	from, ok := p.sticky[session]
	if !ok || p.pinned != "" {
		return ColdUnpin{}, false
	}
	delete(p.sticky, session)
	return ColdUnpin{Trigger: trigger, From: from}, true
}

// evictColdLocked keeps p.cold bounded. Entries idle past the longest TTL
// carry no information (any request would be idle-cold anyway) and go first;
// if still full, the least recently used quarter goes. Runs only when a new
// key would exceed maxColdKeys.
func (p *Pool) evictColdLocked(now time.Time) {
	if len(p.cold) < maxColdKeys {
		return
	}
	for k, s := range p.cold {
		if now.Sub(s.lastUse) > longCacheTTL {
			delete(p.cold, k)
		}
	}
	if len(p.cold) < maxColdKeys {
		return
	}
	keys := make([]string, 0, len(p.cold))
	for k := range p.cold {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return p.cold[keys[i]].lastUse.Before(p.cold[keys[j]].lastUse) })
	for _, k := range keys[:len(keys)/4+1] {
		delete(p.cold, k)
	}
}
