package tool

import "encoding/json"

// IsReadOnly reports whether a tool call with the given name and arguments is
// read-only, suitable for stream-time execution. bash is judged by its
// arguments; other tools by their Caps declaration.
func IsReadOnly(name string, args json.RawMessage) bool {
	if name == "bash" {
		return BashCallIsReadOnly(args)
	}
	caps, ok := CapsByName(name)
	if !ok {
		return false
	}
	return caps.ReadOnly
}
