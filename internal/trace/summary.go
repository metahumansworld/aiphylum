package trace

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Summary renders one event's payload as a compact single line: the keys that
// read like a sentence first, the rest in stable order. It is the shared
// wording for every human-facing event log — dungeonctl's trace listing and
// the web replay viewer — so the same event never reads two ways.
func Summary(l Line) string {
	var m map[string]any
	if err := json.Unmarshal(l.Payload, &m); err != nil {
		return string(l.Payload)
	}
	lead := []string{"action", "id", "agent", "bounty", "winner", "note", "reason"}
	var parts []string
	seen := map[string]bool{}
	for _, k := range lead {
		if v, ok := m[k]; ok {
			parts = append(parts, fmt.Sprintf("%s=%s", k, compactValue(v)))
			seen[k] = true
		}
	}
	var rest []string
	for k := range m {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		parts = append(parts, fmt.Sprintf("%s=%s", k, compactValue(m[k])))
	}
	return strings.Join(parts, " ")
}

// compactValue keeps values one-line and short; long strings and nested
// structures are truncated rather than dumped.
func compactValue(v any) string {
	switch t := v.(type) {
	case string:
		if len(t) > 48 {
			return fmt.Sprintf("%q…", t[:48])
		}
		return fmt.Sprintf("%q", t)
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprint(int64(t))
		}
		return fmt.Sprint(t)
	case bool:
		return fmt.Sprint(t)
	case nil:
		return "null"
	default:
		enc, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprintf("%v", t)
		}
		if len(enc) > 64 {
			return string(enc[:64]) + "…"
		}
		return string(enc)
	}
}
