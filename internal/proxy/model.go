package proxy

// modelMap rewrite — design doc §4 allowed mutations #3 and #4, cross-provider
// only (§6.12). When a request is served through an account whose provider
// speaks different model IDs (e.g. Kimi), spillway rewrites:
//
//   #3 the body's top-level "model" string — Claude Code's executor model.
//   #4 any "model" string that is a direct field of an element of the
//      top-level "tools[]" array — Claude Code's advisor models (issue #29).
//      Claude Code puts an advisor's model there rather than at the top
//      level, and without this it went upstream unrewritten: on rotation to
//      a non-Claude account the executor model got mapped while the advisor
//      model reached the provider verbatim, silently asking it for a model
//      it never advertised.
//
// Both use the SAME modelMap — one mapping, not two — and both are subject
// to §6.12: an unmapped model is a HARD error, never a silent passthrough.
// Nothing outside tools[] is touched: a "model" string typed into ordinary
// message content, or nested deeper inside a tool's own schema (e.g.
// input_schema.properties.model), is left exactly as the client sent it.
//
// Mutation #4 locates nested values with a hand-rolled byte scanner
// (findNestedToolModelValues) while decoding their actual values with
// encoding/json (probe.Tools) — the same split findTopLevelModelValue and
// probe.Model already use for mutation #3. A malformed or adversarial body
// can make the two disagree (duplicate JSON keys, an escaped key our
// scanner's literal comparison won't unescape): rewriteModel treats any
// such disagreement as a hard error rather than guessing, on the same
// fail-closed principle as an unmapped model — see the guards immediately
// before the tools[] loop below, and nested_model_test.go for one example
// of each case.

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// errUnmappedModel marks a model id with no modelMap entry. where is empty
// for the top-level model (mutation #3) or names the tools[] location for a
// nested one (mutation #4), e.g. "tools[2].model".
type errUnmappedModel struct {
	model string
	where string
}

func (e *errUnmappedModel) Error() string {
	if e.where == "" {
		return fmt.Sprintf("spillway: model %q has no modelMap entry for this account — refusing to forward it unmapped", e.model)
	}
	return fmt.Sprintf("spillway: model %q (found in %s) has no modelMap entry for this account — refusing to forward it unmapped", e.model, e.where)
}

// modelOf reads the top-level model field, or "" when absent/malformed. Used
// for reporting only — never for routing. Deliberately does not look inside
// tools[]: per-family gating (issue #24) classifies by this value today, and
// extending it to notice an advisor model nested in tools[] is a routing
// change tracked separately from this mutation fix.
func modelOf(body []byte) string {
	var probe struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &probe) != nil {
		return ""
	}
	return probe.Model
}

// lookupModel resolves a model id: exact match first, then glob patterns
// (§6.12), so `claude-*` can cover a family without listing every version.
// Exact always wins, and among globs the longest pattern wins — otherwise
// adding a broad catch-all would silently capture ids a specific pattern was
// already handling.
func lookupModel(modelMap map[string]string, model string) (string, bool) {
	if v, ok := modelMap[model]; ok {
		return v, true
	}
	best, bestLen := "", -1
	for pattern, v := range modelMap {
		if !strings.ContainsAny(pattern, "*?[") {
			continue
		}
		if ok, err := filepath.Match(pattern, model); err == nil && ok {
			if len(pattern) > bestLen {
				best, bestLen = v, len(pattern)
			}
		}
	}
	return best, bestLen >= 0
}

// modelEdit is a byte-preserving replacement of one JSON string value.
// start is the index of the opening quote, end the index of the closing
// quote (both as returned by findTopLevelModelValue /
// findNestedToolModelValues), and value is the raw replacement bytes to
// place between them.
type modelEdit struct {
	start, end int
	value      string
}

// rewriteModel returns body with the top-level "model" value AND any nested
// tools[].model value replaced per modelMap, preserving all other bytes.
// Errors on unmapped models (top-level or nested), a missing top-level model
// field, and malformed bodies.
func rewriteModel(body []byte, modelMap map[string]string) ([]byte, error) {
	var probe struct {
		Model string `json:"model"`
		Tools []struct {
			Model string `json:"model"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, fmt.Errorf("spillway: malformed request body: %w", err)
	}
	if probe.Model == "" {
		return nil, errors.New("spillway: request body has no top-level model field")
	}
	mapped, ok := lookupModel(modelMap, probe.Model)
	if !ok {
		return nil, &errUnmappedModel{model: probe.Model}
	}

	var edits []modelEdit
	if mapped != probe.Model {
		start := findTopLevelModelValue(body)
		if start < 0 {
			return nil, errors.New("spillway: could not locate top-level model value")
		}
		end, err := findStringValueEnd(body, start)
		if err != nil {
			return nil, err
		}
		edits = append(edits, modelEdit{start: start, end: end, value: mapped})
	}

	// Mutation #4: nested advisor models under tools[]. Same map, same hard
	// error on an unmapped id (§6.12) — the whole point is that one cannot
	// escape mapping by hiding inside tools[].
	//
	// Both findNestedToolModelValues (a byte scanner) and probe.Tools (an
	// encoding/json decode) parse the same bytes independently, and a
	// malformed or adversarial body can make them disagree about what
	// tools[] actually contains — a duplicate top-level "tools" key, a
	// duplicate "model" key inside one element, or a key spelled with a
	// JSON escape our scanner's literal byte comparison doesn't unescape.
	// Any such disagreement means we cannot be confident which bytes are
	// which model, so — per the same §6.12 fail-closed stance an unmapped
	// model gets — this refuses to forward rather than guessing. See
	// nested_model_test.go for one worked example of each case this guards.
	if n := countTopLevelKey(body, "tools"); n > 1 {
		return nil, fmt.Errorf("spillway: request body has %d top-level %q keys — refusing to forward under that structural ambiguity", n, "tools")
	}
	sites := findNestedToolModelValues(body)
	siteCount := make(map[int]int, len(sites))
	for _, site := range sites {
		siteCount[site.toolIndex]++
	}
	for i, tool := range probe.Tools {
		if tool.Model != "" && siteCount[i] == 0 {
			return nil, fmt.Errorf("spillway: tools[%d].model %q was decoded from the body but the byte scanner could not locate it (a non-literal or escaped key?) — refusing to forward under that disagreement", i, tool.Model)
		}
	}
	seen := make(map[int]bool, len(sites))
	for _, site := range sites {
		if site.toolIndex < 0 || site.toolIndex >= len(probe.Tools) {
			return nil, fmt.Errorf("spillway: nested model scan found tools[%d].model but the body decoded only %d tools[] entries — refusing to forward under that disagreement", site.toolIndex, len(probe.Tools))
		}
		if seen[site.toolIndex] {
			return nil, fmt.Errorf("spillway: tools[%d] has more than one %q key — refusing to forward under that structural ambiguity", site.toolIndex, "model")
		}
		seen[site.toolIndex] = true

		nestedModel := probe.Tools[site.toolIndex].Model
		nestedMapped, ok := lookupModel(modelMap, nestedModel)
		if !ok {
			return nil, &errUnmappedModel{
				model: nestedModel,
				where: fmt.Sprintf("tools[%d].model", site.toolIndex),
			}
		}
		if nestedMapped == nestedModel {
			continue
		}
		edits = append(edits, modelEdit{start: site.start, end: site.end, value: nestedMapped})
	}

	if len(edits) == 0 {
		return body, nil
	}
	return applyModelEdits(body, edits), nil
}

// findStringValueEnd scans forward from the opening quote at start (as
// returned by findTopLevelModelValue) and returns the index of the closing
// quote.
func findStringValueEnd(body []byte, start int) (int, error) {
	end := start + 1
	for end < len(body) {
		if body[end] == '\\' {
			end += 2
			continue
		}
		if body[end] == '"' {
			return end, nil
		}
		end++
	}
	return 0, errors.New("spillway: unterminated model value")
}

// applyModelEdits applies non-overlapping edits (each [start,end] inclusive
// of both surrounding quotes) to body in one pass, preserving every byte not
// covered by an edit.
func applyModelEdits(body []byte, edits []modelEdit) []byte {
	sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
	out := make([]byte, 0, len(body))
	pos := 0
	for _, e := range edits {
		out = append(out, body[pos:e.start+1]...) // through the opening quote
		out = append(out, e.value...)
		pos = e.end // resume at the closing quote
	}
	out = append(out, body[pos:]...)
	return out
}

// countTopLevelKey returns how many times a key named "name" appears as a
// direct key of the root JSON object, using the same key/value tracking
// convention as findTopLevelModelValue (a string is a candidate key exactly
// when no key has been recorded yet for the current top-level pair).
//
// This exists to detect a duplicate top-level "tools" key: JSON permits
// duplicate keys, encoding/json keeps the LAST occurrence when decoding
// probe.Tools, and without this check a scanner match against an EARLIER
// occurrence could be paired with the wrong (last-decoded) tool, either
// panicking on an out-of-range index or — worse — silently splicing one
// tool's mapped model into a different tool's byte span.
func countTopLevelKey(body []byte, name string) int {
	depth := 0
	inStr := false
	esc := false
	readingKey := false
	keyStart := 0
	key := ""
	count := 0
	for i := 0; i < len(body); i++ {
		b := body[i]
		if inStr {
			if esc {
				esc = false
				continue
			}
			if b == '\\' {
				esc = true
				continue
			}
			if b == '"' {
				inStr = false
				if readingKey {
					key = string(body[keyStart:i])
					readingKey = false
					if depth == 1 && key == name {
						count++
					}
				}
			}
			continue
		}
		switch b {
		case '{':
			depth++
		case '}':
			depth--
		case '"':
			inStr = true
			if depth == 1 && key == "" {
				readingKey = true
				keyStart = i + 1
			}
		case ',':
			if depth == 1 {
				key = ""
			}
		}
	}
	return count
}

// findTopLevelModelValue locates the value of the top-level "model" key,
// tracking string/escape state and container depth. Returns the index of the
// value's opening quote, or -1.
func findTopLevelModelValue(body []byte) int {
	depth := 0
	inStr := false
	esc := false
	readingKey := false
	keyStart := 0
	var key string
	for i := 0; i < len(body); i++ {
		b := body[i]
		if inStr {
			if esc {
				esc = false
				continue
			}
			if b == '\\' {
				esc = true
				continue
			}
			if b == '"' {
				inStr = false
				if readingKey {
					key = string(body[keyStart:i])
					readingKey = false
				}
			}
			continue
		}
		switch b {
		case '{':
			depth++
		case '}':
			depth--
		case '"':
			inStr = true
			if depth == 1 && key == "" {
				// candidate key at top level
				readingKey = true
				keyStart = i + 1
			} else if depth == 1 && key == "model" && body[i-1] != '\\' {
				// string value position — but only when the previous
				// non-space byte is ':'.
				j := i - 1
				for j >= 0 && (body[j] == ' ' || body[j] == '\t' || body[j] == '\r' || body[j] == '\n') {
					j--
				}
				if j >= 0 && body[j] == ':' {
					return i
				}
			}
		case ',':
			if depth == 1 {
				key = "" // next key
			}
		}
	}
	return -1
}

// nestedModelSite is one occurrence of a "model" key that is a direct field
// of an object which is itself a direct element of the top-level "tools[]"
// array. start/end are the indices of the value's surrounding quotes, in the
// same convention as findTopLevelModelValue. toolIndex is the element's
// 0-based position within tools[], used both to cross-reference the decoded
// value (via encoding/json, so escapes are handled correctly) and to name the
// location in an unmapped-model error.
type nestedModelSite struct {
	start, end int
	toolIndex  int
}

// findNestedToolModelValues scans body for "model" keys scoped to tools[]
// exactly as described above. It deliberately does NOT recurse into a tool's
// own nested structures — a JSON-schema property happening to be named
// "model" (e.g. inside input_schema.properties) sits two or more object
// levels below the tools[] element itself and is never matched — and it
// never looks outside the top-level "tools" key, so a "model" string typed
// into a user message, or a same-named key nested elsewhere in the body, is
// never touched.
func findNestedToolModelValues(body []byte) []nestedModelSite {
	type frame struct {
		isArray       bool
		isToolsArray  bool // this array IS the top-level tools[] array
		isToolElement bool // this object is a direct element of tools[]
		key           string
		awaitingValue bool
		index         int // array frames only: 0-based element count so far
	}
	var stack []frame
	var sites []nestedModelSite

	inStr := false
	esc := false
	isKey := false
	keyStart := 0
	capturing := false
	pendingStart := 0
	pendingToolIndex := 0

	for i := 0; i < len(body); i++ {
		b := body[i]
		if inStr {
			if esc {
				esc = false
				continue
			}
			if b == '\\' {
				esc = true
				continue
			}
			if b == '"' {
				inStr = false
				if isKey {
					isKey = false
					if n := len(stack); n > 0 && !stack[n-1].isArray {
						stack[n-1].key = string(body[keyStart:i])
					}
				} else if capturing {
					sites = append(sites, nestedModelSite{start: pendingStart, end: i, toolIndex: pendingToolIndex})
					capturing = false
				}
			}
			continue
		}
		switch b {
		case '"':
			inStr = true
			n := len(stack)
			if n == 0 {
				continue
			}
			top := n - 1
			switch {
			case stack[top].isArray:
				// A bare string array element — never a key, never our target.
			case !stack[top].awaitingValue:
				isKey = true
				keyStart = i + 1
			default:
				if stack[top].isToolElement && stack[top].key == "model" {
					capturing = true
					pendingStart = i
					if top >= 1 {
						pendingToolIndex = stack[top-1].index
					}
				}
				stack[top].awaitingValue = false
				stack[top].key = ""
			}
		case '{':
			isElem := false
			if n := len(stack); n > 0 && stack[n-1].isArray && stack[n-1].isToolsArray {
				isElem = true
			}
			if n := len(stack); n > 0 && !stack[n-1].isArray && stack[n-1].awaitingValue {
				stack[n-1].awaitingValue = false
				stack[n-1].key = ""
			}
			stack = append(stack, frame{isToolElement: isElem})
		case '}':
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		case '[':
			isTools := false
			if n := len(stack); n == 1 && !stack[n-1].isArray && stack[n-1].awaitingValue && stack[n-1].key == "tools" {
				isTools = true
			}
			if n := len(stack); n > 0 && !stack[n-1].isArray && stack[n-1].awaitingValue {
				stack[n-1].awaitingValue = false
				stack[n-1].key = ""
			}
			stack = append(stack, frame{isArray: true, isToolsArray: isTools})
		case ']':
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		case ':':
			if n := len(stack); n > 0 && !stack[n-1].isArray && stack[n-1].key != "" {
				stack[n-1].awaitingValue = true
			}
		case ',':
			if n := len(stack); n > 0 {
				if stack[n-1].isArray {
					stack[n-1].index++
				} else {
					stack[n-1].key = ""
					stack[n-1].awaitingValue = false
				}
			}
		}
	}
	return sites
}
