package proxy

// modelMap rewrite — design doc §4 allowed mutation #3, cross-provider only
// (§6.12): when a request is served through an account whose provider speaks
// different model IDs (e.g. Kimi), the body's top-level "model" string is
// rewritten via the account's modelMap. An unmapped model is a HARD error —
// never silently pass a claude model id to another provider.

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// errUnmappedModel marks a model id with no modelMap entry.
type errUnmappedModel struct{ model string }

func (e *errUnmappedModel) Error() string {
	return fmt.Sprintf("spillway: model %q has no modelMap entry for this account — refusing to forward it unmapped", e.model)
}

// modelOf reads the top-level model field, or "" when absent/malformed. Used
// for reporting only — never for routing.
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

// rewriteModel returns body with the top-level "model" value replaced per
// modelMap, preserving all other bytes. Errors on unmapped models, missing
// model fields, and malformed bodies.
func rewriteModel(body []byte, modelMap map[string]string) ([]byte, error) {
	var probe struct {
		Model string `json:"model"`
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
	if mapped == probe.Model {
		return body, nil
	}

	// Byte-preserving replacement of the first top-level "model":"<value>".
	start := findTopLevelModelValue(body)
	if start < 0 {
		return nil, errors.New("spillway: could not locate top-level model value")
	}
	// start points at the opening quote of the value string.
	end := start + 1
	for end < len(body) {
		if body[end] == '\\' {
			end += 2
			continue
		}
		if body[end] == '"' {
			break
		}
		end++
	}
	if end >= len(body) {
		return nil, errors.New("spillway: unterminated model value")
	}
	out := make([]byte, 0, len(body)+len(mapped)-len(probe.Model))
	out = append(out, body[:start+1]...)
	out = append(out, mapped...)
	out = append(out, body[end:]...)
	return out, nil
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
