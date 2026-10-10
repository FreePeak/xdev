package agent

import (
	"fmt"
	"strings"

	"github.com/FreePeak/xdev/internal/tool"
)

// The message a tool call gets when its name is not in the registry.
//
// Measured 2026-10-10 over every stored session on this box (275 session
// files): 26 calls reached runOneTool's miss branch, and 17 of them were
// the SAME garbled name — `ash_edit`, which is `ast_edit` with its `t`
// dropped — emitted by two providers (onegw/opencode-step-5-preview-free,
// onegw/planning) across three separate repositories. The model's own
// thinking in one of those sessions names it: "I mistyped the tool name
// twice — it's `ast_edit`". The other nine were fishing: `ash_g1`..
// `ash_g4`, `ash_x`, `ash_y`, `ash_q`..`ash_w`, each with `{}` for
// arguments, emitted after an earlier call failed. Three more of the 26
// were the NUL splice #599 already handles at the top of runOneTool.
//
// The old text was four words — `unknown tool "ash_g2"` — which names
// what is NOT there and tells the model nothing to do, so a model that
// garbled one name emitted nine more, each costing a full round trip. The
// catalog already learned this at its own seam: internal/tool's missText
// distinguishes "registered but not deferred" from "unknown" and lists the
// catalog, because `unknown tool "web_search"` once sent a model to curl
// instead of using its own tool. This is the same answer for the
// direct-lookup path.
//
// A near-miss NAMES a candidate; it never runs one. A model that garbled
// `write` into `wriet` is told the name and re-sends it, so the call takes
// the ordinary path — plan-mode gate, approval policy, interceptor chain,
// hooks — instead of the harness guessing which tool a mangled name meant
// and executing it.
func unknownToolText(reg *tool.Registry, name string) string {
	if near, ok := nearToolName(reg, name); ok {
		return fmt.Sprintf("unknown tool %q; the closest registered tool is %q — resend the call as %q",
			name, near, near)
	}
	msg := fmt.Sprintf("unknown tool %q; no tool by that name exists in this session. "+
		"Do not invent tool names: call a tool you already have, or %s <query> to look one up",
		name, tool.ToolSearchName)
	if list := deferredToolList(reg); list != "" {
		msg += " (the deferred catalog holds: " + list + ")"
	}
	return msg
}

// nearToolName is the registry's answer to "which real tool did this
// garble come from?", or false when nothing is close enough. The rule is
// fallback_chain.go's model-id rule, reused verbatim so the repo keeps one
// near-miss rule: edit distance at most 2 with a length delta at most 2.
// Names() is sorted and the scan keeps the first of the shortest, so the
// answer is deterministic — a resumed session gets the suggestion it got
// before.
func nearToolName(reg *tool.Registry, name string) (string, bool) {
	if reg == nil || name == "" {
		return "", false
	}
	best, bestDist := "", -1
	for _, n := range reg.Names() {
		d := editDistance(name, n)
		if d > 2 || abs(len(name)-len(n)) > 2 {
			continue
		}
		if bestDist < 0 || d < bestDist {
			best, bestDist = n, d
		}
	}
	return best, bestDist >= 0
}

// deferredToolList names the tools this registry keeps behind the bridge,
// comma-separated, or "" when the catalog is empty. The eager ones are
// already in the model's tool schema, so naming them again would be noise;
// the deferred ones are not, and this is where a model reads what it is
// missing.
func deferredToolList(reg *tool.Registry) string {
	entries := reg.Deferred()
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name)
	}
	return strings.Join(names, ", ")
}
