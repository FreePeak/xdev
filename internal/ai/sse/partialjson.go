package sse

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ParsePartial repairs a truncated JSON document the way omp's relaxed
// parser does: incomplete trailing strings/numbers/literals are closed or
// dropped, and open containers are closed, so streaming deltas never crash
// mid-arguments. The final strict parse at toolcall_end stays authoritative
// (adapters call json.Unmarshal directly for that).
func ParsePartial(data string) (json.RawMessage, error) {
	p := &repairer{src: []byte(data)}
	out, err := p.run()
	if err != nil {
		return nil, err
	}
	// Sanity: result must be valid JSON.
	if !json.Valid(out) {
		return nil, fmt.Errorf("sse: repaired JSON still invalid")
	}
	return json.RawMessage(out), nil
}

// ErrEmptyPartial is returned for empty/whitespace input.
var ErrEmptyPartial = errors.New("sse: empty partial JSON")

type repairer struct {
	src []byte
	pos int
}

func (p *repairer) run() ([]byte, error) {
	p.skipWS()
	if p.pos >= len(p.src) {
		return nil, ErrEmptyPartial
	}
	v, err := p.value()
	if err != nil {
		return nil, err
	}
	out := append([]byte(nil), v...)
	// Trailing garbage after a complete top-level value: drop it.
	return out, nil
}

func (p *repairer) skipWS() {
	for p.pos < len(p.src) {
		switch p.src[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *repairer) value() ([]byte, error) {
	if p.pos >= len(p.src) {
		return nil, nil
	}
	switch c := p.src[p.pos]; {
	case c == '{':
		return p.object()
	case c == '[':
		return p.array()
	case c == '"':
		return p.string()
	case c == 't':
		return p.literal("true"), nil
	case c == 'f':
		return p.literal("false"), nil
	case c == 'n':
		return p.literal("null"), nil
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number(), nil
	default:
		return nil, fmt.Errorf("sse: unexpected byte %q at %d", c, p.pos)
	}
}

func (p *repairer) literal(word string) []byte {
	for i := range len(word) {
		if p.pos >= len(p.src) || p.src[p.pos] != word[i] {
			return []byte("null") // truncated literal like "tru"
		}
		p.pos++
	}
	return []byte(word)
}

func (p *repairer) number() []byte {
	start := p.pos
	if p.pos < len(p.src) && p.src[p.pos] == '-' {
		p.pos++
	}
	digits := 0
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if (c >= '0' && c <= '9') || c == '.' || c == 'e' || c == 'E' || c == '+' || c == '-' {
			if c >= '0' && c <= '9' {
				digits++
			}
			p.pos++
			continue
		}
		break
	}
	frag := p.src[start:p.pos]
	// Truncated exponent ("1.5e") or bare sign/dot: salvage as integer.
	clean := make([]byte, 0, len(frag))
	for _, c := range frag {
		if (c >= '0' && c <= '9') || c == '-' || c == '.' {
			clean = append(clean, c)
		}
	}
	if digits == 0 || len(clean) == 0 {
		return []byte("0")
	}
	return clean
}

// string returns a complete JSON string for the string starting at p.pos,
// repairing truncation by dropping a dangling escape or closing the quote.
func (p *repairer) string() ([]byte, error) {
	out := []byte{'"'}
	p.pos++ // opening quote
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == '\\' {
			if p.pos+1 >= len(p.src) {
				return append(out, '"'), nil // dangling backslash: drop it
			}
			out = append(out, c, p.src[p.pos+1])
			p.pos += 2
			continue
		}
		if c == '"' {
			out = append(out, '"')
			p.pos++
			return out, nil
		}
		out = append(out, c)
		p.pos++
	}
	return append(out, '"'), nil // EOF inside string: close it
}

func (p *repairer) object() ([]byte, error) {
	out := []byte{'{'}
	p.pos++ // '{'
	first := true
	for {
		p.skipWS()
		if p.pos >= len(p.src) {
			return p.closeAll(out, '}'), nil
		}
		if p.src[p.pos] == '}' {
			p.pos++
			return append(out, '}'), nil
		}
		if !first {
			if p.src[p.pos] == ',' {
				p.pos++
				p.skipWS()
				if p.pos >= len(p.src) {
					return p.closeAll(out, '}'), nil
				}
			} else {
				// Missing comma: treat as end of parseable region.
				return p.closeAll(out, '}'), nil
			}
		}
		if p.src[p.pos] == '}' {
			p.pos++
			return append(out, '}'), nil
		}
		if p.src[p.pos] != '"' {
			// Can't parse a key here (e.g. `"key":` then EOF is fine, but
			// garbage means stop) — stop and close.
			return p.closeAll(out, '}'), nil
		}
		key, err := p.string()
		if err != nil {
			return nil, err
		}
		if len(key) < 2 {
			return p.closeAll(out, '}'), nil
		}
		p.skipWS()
		if p.pos >= len(p.src) {
			// Key without value: omit the key entirely.
			return p.closeAll(out, '}'), nil
		}
		if p.src[p.pos] != ':' {
			return p.closeAll(out, '}'), nil
		}
		p.pos++
		p.skipWS()
		var val []byte
		if p.pos < len(p.src) {
			val, err = p.value()
			if err != nil {
				return nil, err
			}
		}
		if val == nil || p.pos >= len(p.src) && valIncomplete(val) {
			// Truncated value: omit key/value pair.
			return p.closeAll(out, '}'), nil
		}
		if !first {
			out = append(out, ',')
		}
		out = append(out, key...)
		out = append(out, ':')
		out = append(out, val...)
		first = false
	}
}

func (p *repairer) array() ([]byte, error) {
	out := []byte{'['}
	p.pos++ // '['
	first := true
	for {
		p.skipWS()
		if p.pos >= len(p.src) {
			return p.closeAll(out, ']'), nil
		}
		if p.src[p.pos] == ']' {
			p.pos++
			return append(out, ']'), nil
		}
		if !first {
			if p.src[p.pos] == ',' {
				p.pos++
				p.skipWS()
				if p.pos >= len(p.src) {
					return p.closeAll(out, ']'), nil
				}
			} else {
				return p.closeAll(out, ']'), nil
			}
		}
		if p.src[p.pos] == ']' {
			p.pos++
			return append(out, ']'), nil
		}
		val, err := p.value()
		if err != nil {
			return nil, err
		}
		if val == nil || p.pos >= len(p.src) && valIncomplete(val) {
			return p.closeAll(out, ']'), nil
		}
		if !first {
			out = append(out, ',')
		}
		out = append(out, val...)
		first = false
	}
}

// closeAll closes the trailing container: drop a dangling comma, append the
// closer. Deeper unclosed containers cannot occur: object/array recurse and
// each returns a balanced fragment.
func (p *repairer) closeAll(out []byte, closer byte) []byte {
	if len(out) > 1 && (out[len(out)-1] == ',' || out[len(out)-1] == ':') {
		out = out[:len(out)-1]
	}
	return append(out, closer)
}

// valIncomplete reports whether a value fragment at EOF is truncated in a
// way that should make the caller drop it (dangling number fragment).
func valIncomplete(val []byte) bool {
	if len(val) == 0 {
		return true
	}
	c := val[len(val)-1]
	return c == '-' || c == '+' || c == '.'
}
