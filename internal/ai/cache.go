package ai

// Prompt caching (M9 #133).
//
// A provider bills the repeated prefix of a request — tools, then system
// prompt, then the conversation — at a fraction of the input rate, but only up
// to a `cache_control` marker: with no marker nothing is written to the cache,
// and every turn pays full price and full read latency. xdev parsed
// cache_read/cache_creation usage and emitted no marker at all, so those
// counters could never move.
//
// The shape is omp's, read out of the shipped binary
// (packages/ai/src/providers/anthropic.ts, the DHr/GHr/LHr/FHr family):
//   - the system prompt rides as blocks so it can carry a marker;
//   - one marker on the LAST tool definition: on this wire the cache is
//     prefix-ordered tools → system → messages, so a marker on the last tool
//     caches the tools and a marker on the last system block caches
//     tools+system in one write;
//   - then a rolling conversation tail, newest first, plus every 15th markable
//     message behind it — a deep marker survives a tail rewrite;
//   - Anthropic's budget is 4 markers; unused budget is simply not spent;
//   - retention is {"type":"ephemeral"} (5m) by default and ttl "1h" only when
//     XDEV_CACHE_RETENTION/PI_CACHE_RETENTION asks for long; "none" turns
//     caching off.
//
// What omp does that xdev has no equivalent of, and so does not copy:
// mid-conversation system messages, per-message effort,
// tool_addition/tool_removal blocks, and OAuth-subscription TTL gating.
//
// A marker the endpoint cannot parse is an error, not a cache miss, and not
// every anthropic-messages provider is Anthropic — so a 400 that names
// cache_control latches markers off for the rest of the process.

import (
	"errors"
	"hash/fnv"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	"github.com/FreePeak/xdev/internal/logx"
)

// cacheMaxBreakpoints is Anthropic's documented marker budget (4).
const cacheMaxBreakpoints = 4

// cacheTailPeriod marks every Nth older markable message behind the newest one
// (omp NHr): deep enough to be worth a write, sparse enough to leave budget.
const cacheTailPeriod = 15

// cacheMarkersDisabled latches after a gateway rejects the field. One bad 400
// must not keep killing every turn of every session in the process.
var cacheMarkersDisabled atomic.Bool

// cacheRetention resolves the TTL bucket: XDEV_CACHE_RETENTION wins over omp's
// PI_CACHE_RETENTION, and anything unrecognised — including omp's own "auto" —
// is the default short/5m bucket. Read per call rather than cached: it is a
// couple of map lookups on a path that builds one request per turn, and a
// process-lifetime Once would make the override untestable.
func cacheRetention() string {
	for _, key := range []string{"XDEV_CACHE_RETENTION", "PI_CACHE_RETENTION"} {
		switch v := strings.ToLower(strings.TrimSpace(os.Getenv(key))); v {
		case "long", "short", "none":
			return v
		}
	}
	return "short"
}

// cacheEnabled reports whether markers should be emitted: off for retention
// "none" (which also drops the OpenAI cache-affinity key) and off once a
// gateway has rejected them.
func cacheEnabled() bool {
	return cacheRetention() != "none" && !cacheMarkersDisabled.Load()
}

// disableCacheMarkers latches markers off for the process and says so once:
// silently falling back is how the next reader ends up wondering why
// cache_read stayed 0 forever.
func disableCacheMarkers(why string) {
	if cacheMarkersDisabled.Swap(true) {
		return
	}
	logx.Errorf("prompt caching: %s; cache_control markers disabled for this process (set XDEV_CACHE_RETENTION=none to opt out up front)", why)
}

// CacheControl is Anthropic's cache marker: {"type":"ephemeral","ttl":"1h"}.
// It sits on the wire element that ENDS a cached span — everything before it,
// inclusive, is the prefix that gets written and billed as a read later.
type CacheControl struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}

// cacheMarker returns a fresh marker for the resolved retention. Fresh, not
// shared: markers land in distinct wire blocks, and one pointer written into
// all of them makes a request body impossible to reason about.
func cacheMarker() *CacheControl {
	cc := &CacheControl{Type: "ephemeral"}
	if cacheRetention() == "long" {
		cc.TTL = "1h"
	}
	return cc
}

// applyAnthropicBreakpoints spends the marker budget the system blocks and the
// tool schema did not use, walking the conversation from the newest markable
// message backwards and then every cacheTailPeriod-th one behind it.
func applyAnthropicBreakpoints(msgs []anthropicWireMessage, budget int) {
	if budget <= 0 {
		return
	}
	markable := make([]int, 0, len(msgs))
	for i := range msgs {
		if anthropicCacheMarkable(msgs[i]) {
			markable = append(markable, i)
		}
	}
	if len(markable) == 0 {
		return
	}
	// Newest first, then the periodic deep markers (deepest first, so the one
	// that costs the most to rewrite is placed before the budget runs out),
	// then the second-newest if budget is still open.
	order := []int{markable[len(markable)-1]}
	for n := len(markable) - 1 - cacheTailPeriod; n >= 0; n -= cacheTailPeriod {
		order = append(order, markable[n])
	}
	if len(markable) > 1 {
		order = append(order, markable[len(markable)-2])
	}
	placed := 0
	for _, i := range order {
		if placed >= budget {
			return
		}
		// One marker per message is the budget unit, and a fresh pointer per
		// block keeps each wire element independently inspectable.
		if markAnthropicMessage(&msgs[i], cacheMarker()) {
			placed++
		}
	}
}

// anthropicCacheMarkable reports whether a wire message can end a cached span.
// Only user and assistant turns qualify; a tool_result merged into a user
// message is exactly what a warm tail should cache.
func anthropicCacheMarkable(m anthropicWireMessage) bool {
	if m.Role != "user" && m.Role != "assistant" {
		return false
	}
	for _, b := range m.Content {
		switch b.Type {
		case "text", "tool_result", "tool_use", "image":
			return true
		}
	}
	return false
}

// markAnthropicMessage attaches a marker to the last cacheable block of a
// message. False when nothing in it can carry one — thinking is skipped on
// purpose: an unsigned replayed thinking block is rejected by the endpoint, so
// a marker there would fail the request instead of merely missing the cache
// (omp FHr skips thinking/redacted_thinking for the same reason).
func markAnthropicMessage(m *anthropicWireMessage, marker *CacheControl) bool {
	for i := len(m.Content) - 1; i >= 0; i-- {
		b := &m.Content[i]
		switch b.Type {
		case "thinking", "redacted_thinking":
			continue
		case "text", "tool_result", "tool_use", "image":
		default:
			continue
		}
		if b.CacheControl == nil {
			b.CacheControl = marker
		}
		return true
	}
	return false
}

// cacheAffinityKey derives the OpenAI `prompt_cache_key` from a session id:
// the id verbatim within the wire's 64-character ceiling, else a stable hash
// under a "pc_" prefix (omp openai-shared). Empty in — or caching off — means
// no key is sent, which is the pre-cache behavior, not a regression.
func cacheAffinityKey(in string) string {
	in = strings.TrimSpace(in)
	if in == "" || cacheRetention() == "none" {
		return ""
	}
	if utf8.RuneCountInString(in) <= 64 {
		return in
	}
	return "pc_" + shortHash(in)
}

// shortHash renders s's 64-bit FNV-1a in base36: stable and short, so an
// over-long session id routes to ONE cache bucket instead of several.
func shortHash(s string) string {
	h := fnv.New64a()
	h.Write([]byte(s))
	return strconv.FormatUint(h.Sum64(), 36)
}

// cacheRejected reports the one error class a marker can cause: a 4xx that
// names the field. A provider that rejects the value outright never means
// "temporarily unavailable", and treating it as anything else re-sends a body
// the endpoint has already told us it cannot parse.
func cacheRejected(err error) bool {
	var he *HTTPError
	if !errors.As(err, &he) {
		return false
	}
	return he.Status >= 400 && he.Status < 500 && strings.Contains(strings.ToLower(he.Body), "cache_control")
}

// promptCacheKey is the OpenAI family's request-level cache affinity: the
// session key, and only for a first-party endpoint. OpenAI-compatible proxies
// reject an unknown top-level field with a 400, and xdev has no per-model
// compat table to ask about it (omp has one: compat.supportsPromptCacheKey) —
// so the host is the honest test. localhost is deliberately not one: a local
// server has no prompt cache to route to.
func promptCacheKey(baseURL, sessionKey string) string {
	if sessionKey == "" {
		return ""
	}
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || !strings.EqualFold(u.Hostname(), "api.openai.com") {
		return ""
	}
	return cacheAffinityKey(sessionKey)
}
