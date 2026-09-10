package tui

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Declarative extension renderers (M7 #8, PRD §IV.8): an extension
// announces {tool, kind, spec} at the handshake and the host renders
// results accordingly. Custom in-process renderers are the documented
// capability loss vs omp's TS extensions — specs DEGRADE GRACEFULLY:
// any parse failure falls back to the plain text the model already sees.

// RenderSpec is one announced renderer (mirrors ext.Renderer; the view
// package keeps the data type local so it depends on nothing upstream).
type RenderSpec struct {
	Kind string          `json:"kind"` // card | table | tree
	Spec json.RawMessage `json:"spec"`
}

// SetRenderers installs declarative specs keyed by tool name.
func (a *App) SetRenderers(specs map[string]RenderSpec) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.renderers = specs
}

// renderToolOutput formats one tool result per its spec, returning
// ok=false when the spec or payload does not fit (caller keeps raw text).
func renderToolOutput(spec RenderSpec, toolName, raw string) (string, bool) {
	if raw == "" || strings.TrimSpace(raw)[0] != '{' && strings.TrimSpace(raw)[0] != '[' {
		return "", false // not JSON: nothing declarative to render
	}
	switch spec.Kind {
	case "card":
		return renderCard(spec.Spec, toolName, raw)
	case "table":
		return renderTable(spec.Spec, raw)
	case "tree":
		return renderTree(spec.Spec, raw)
	default:
		return "", false // unknown kind: degrade to text
	}
}

// specShape is the declarative subset: field selections by key name.
type specShape struct {
	Title   string   `json:"title"`   // card: field holding the heading
	Fields  []string `json:"fields"`  // card/tree: ordered keys
	Columns []string `json:"columns"` // table: header order
	Items   string   `json:"items"`   // table/tree: field holding the array
}

func decodeSpec(raw json.RawMessage) (specShape, bool) {
	var sh specShape
	if len(raw) == 0 {
		return sh, true // an empty spec means "render whatever is there"
	}
	if err := json.Unmarshal(raw, &sh); err != nil {
		return sh, false
	}
	return sh, true
}

// renderCard: "title" line then key: value rows.
func renderCard(specRaw json.RawMessage, toolName, payload string) (string, bool) {
	sh, ok := decodeSpec(specRaw)
	if !ok {
		return "", false
	}
	obj, ok := asObject(payload)
	if !ok {
		return "", false
	}
	var b strings.Builder
	title := toolName
	titleFromPayload := false
	if sh.Title != "" {
		if v, has := lookup(obj, sh.Title); has {
			title, titleFromPayload = v, true
		}
	}
	fmt.Fprintf(&b, "%s\n", title)
	shown := 0
	for _, f := range cardFields(sh, obj, sh.Title) {
		if v, has := lookup(obj, f); has {
			fmt.Fprintf(&b, "  %s: %s\n", f, v)
			shown++
		}
	}
	// Nothing matched the spec: this is not a render, degrade to text
	// (the model still sees the same payload either way).
	if !titleFromPayload && shown == 0 {
		return "", false
	}
	return strings.TrimRight(b.String(), "\n"), true
}

func cardFields(sh specShape, obj map[string]any, titleKey string) []string {
	if len(sh.Fields) > 0 {
		return sh.Fields
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		if k == titleKey {
			continue // already shown as the heading
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// renderTable: aligned columns over an array of objects.
func renderTable(specRaw json.RawMessage, payload string) (string, bool) {
	sh, ok := decodeSpec(specRaw)
	if !ok {
		return "", false
	}
	rows, cols, ok := tableData(sh, payload)
	if !ok || len(rows) == 0 {
		return "", false
	}
	widths := make([]int, len(cols))
	for i, c := range cols {
		widths[i] = len(c)
		for _, r := range rows {
			if n := len(r[i]); n > widths[i] {
				widths[i] = n
			}
		}
	}
	var b strings.Builder
	writeRow := func(cells []string) {
		for i, cell := range cells {
			if i == len(cells)-1 {
				b.WriteString(cell) // no padding on the final column
				continue
			}
			fmt.Fprintf(&b, "%-*s  ", widths[i], cell)
		}
		b.WriteString("\n")
	}
	writeRow(cols)
	for _, r := range rows {
		writeRow(r)
	}
	return strings.TrimRight(b.String(), "\n"), true
}

func tableData(sh specShape, payload string) (rows [][]string, cols []string, ok bool) {
	candidates := []map[string]any{}
	if arr, isArr := asArray(payload); isArr {
		for _, item := range arr {
			if o, isObj := item.(map[string]any); isObj {
				candidates = append(candidates, o)
			}
		}
	} else if obj, isObj := asObject(payload); isObj {
		items := obj
		if sh.Items != "" {
			v, has := obj[sh.Items]
			if !has {
				return nil, nil, false
			}
			raw, _ := json.Marshal(v)
			list, isArr := asArray(string(raw))
			if !isArr {
				return nil, nil, false
			}
			for _, item := range list {
				if o, isObj := item.(map[string]any); isObj {
					candidates = append(candidates, o)
				}
			}
		} else {
			candidates = append(candidates, items)
		}
	} else {
		return nil, nil, false
	}
	if len(candidates) == 0 {
		return nil, nil, false
	}
	cols = sh.Columns
	if len(cols) == 0 {
		seen := map[string]bool{}
		for _, c := range candidates {
			for k := range c {
				if !seen[k] {
					seen[k] = true
					cols = append(cols, k)
				}
			}
		}
		sort.Strings(cols)
	}
	for _, c := range candidates {
		row := make([]string, len(cols))
		for i, name := range cols {
			v, _ := lookup(c, name)
			row[i] = v
		}
		rows = append(rows, row)
	}
	return rows, cols, true
}

// renderTree: one line per item, indented under an optional root.
func renderTree(specRaw json.RawMessage, payload string) (string, bool) {
	if _, ok := decodeSpec(specRaw); !ok {
		return "", false
	}
	arr, isArr := asArray(payload)
	if !isArr {
		obj, isObj := asObject(payload)
		if !isObj {
			return "", false
		}
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		for _, k := range keys {
			fmt.Fprintf(&b, "%s\n", k)
			if v, is := lookup(obj, k); is && v != "" {
				fmt.Fprintf(&b, "  %s\n", v)
			}
		}
		return strings.TrimRight(b.String(), "\n"), true
	}
	var b strings.Builder
	for _, item := range arr {
		if o, isObj := item.(map[string]any); isObj {
			first := true
			for _, v := range orderedValues(o) {
				indent := "  "
				if first {
					indent = ""
					first = false
				}
				fmt.Fprintf(&b, "%s%v\n", indent, v)
			}
			continue
		}
		fmt.Fprintf(&b, "%v\n", item)
	}
	return strings.TrimRight(b.String(), "\n"), true
}

func orderedValues(o map[string]any) []any {
	keys := make([]string, 0, len(o))
	for k := range o {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]any, 0, len(keys))
	for _, k := range keys {
		out = append(out, o[k])
	}
	return out
}

// lookup resolves a possibly dotted field path to a rendered scalar.
func lookup(obj map[string]any, path string) (string, bool) {
	cur := any(obj)
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", false
		}
		cur, ok = m[part]
		if !ok {
			return "", false
		}
	}
	return scalar(cur), true
}

func scalar(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%g", t)
	case bool:
		return fmt.Sprintf("%v", t)
	default:
		raw, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprintf("%v", t)
		}
		return string(raw)
	}
}

func asObject(payload string) (map[string]any, bool) {
	var m map[string]any
	if err := json.Unmarshal([]byte(payload), &m); err != nil {
		return nil, false
	}
	return m, true
}

func asArray(payload string) ([]any, bool) {
	var a []any
	if err := json.Unmarshal([]byte(payload), &a); err != nil {
		return nil, false
	}
	return a, true
}
