package tool

import (
	"encoding/json"
	"strings"
)

// MatchPermissionSpec reports whether an omp/claude-style permission spec
// matches one tool invocation:
//
//	"Bash"             any bash call
//	"Bash(git *)"      a bash call whose (compound-aware) command matches
//	"Write(src/**)"    a write whose subject argument matches
//
// The bash form delegates to the approval policy's own matchCommand /
// bashCommand, so a hook `if` and the approval rules can never disagree
// about what a pattern means. Non-bash patterns are globbed against the
// invocation's subject argument (the first of path/file_path/pattern/
// command/url/query that is set), falling back to the raw argument JSON.
func MatchPermissionSpec(spec, toolName string, args json.RawMessage) bool {
	name, pattern := parsePermissionSpec(spec)
	if name == "" || !strings.EqualFold(name, toolName) {
		return false
	}
	if pattern == "" {
		return true
	}
	if strings.EqualFold(toolName, "bash") {
		return matchCommand(pattern, bashCommand(args))
	}
	return globMatch(pattern, specSubject(args))
}

// parsePermissionSpec splits "Tool(pattern)" into its parts; a bare "Tool"
// yields an empty pattern.
func parsePermissionSpec(spec string) (name, pattern string) {
	s := strings.TrimSpace(spec)
	if i := strings.IndexByte(s, '('); i >= 0 && strings.HasSuffix(s, ")") {
		return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1 : len(s)-1])
	}
	return s, ""
}

// specSubject is the string a non-bash pattern filters on. Tools carry
// their target under different names; the first set one wins, and a tool
// with no recognizable field falls back to its raw argument JSON.
func specSubject(args json.RawMessage) string {
	var m map[string]any
	if err := json.Unmarshal(args, &m); err != nil {
		return strings.TrimSpace(string(args))
	}
	for _, k := range []string{"path", "file_path", "filePath", "pattern", "command", "url", "query"} {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return strings.TrimSpace(string(args))
}
