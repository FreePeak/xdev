package agent

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/logx"
	"github.com/FreePeak/xdev/internal/session"
)

// Usage-aware fallback, credential rotation, and fallback reversion
// (M5 #25, docs/research/omp-context-resilience §1.6-1.7). The chain
// GRAMMAR lives in fallback_chain.go; this file is the live state: which
// targets are cooling down, which credential is in use, and when the
// primary comes back.
//
// The seams (Rotate, RedeemReset, UsageProbe, Notify) are func fields
// rather than interfaces because the caller — cmd/xdev — owns provider
// construction and the presentation surface, exactly like Approve.

const (
	// transientPin keeps the ladder off a selector that just failed with a
	// transient error for long enough that the next attempt is a real
	// retry, not a bounce.
	transientPin = time.Minute
	// maxRotationsPerProvider bounds credential rotation on one provider:
	// the pool is finite, and a caller-supplied Rotate that always reports
	// success must not spin the turn forever. A chain switch starts a new
	// provider, so the bound resets with it.
	maxRotationsPerProvider = 8
)

// ReservePolicy is the pre-turn reaction to a near-quota target
// (retry.reserveThreshold).
type ReservePolicy uint8

const (
	// ReserveOff never consults usage (the shipped default).
	ReserveOff ReservePolicy = iota
	// ReserveConfirm notifies the user, then moves.
	ReserveConfirm
	// ReserveAuto moves silently.
	ReserveAuto
)

// ParseReservePolicy maps the setting spelling onto the policy. An empty or
// unknown value is off: a typo must never start switching models silently.
func ParseReservePolicy(v string) ReservePolicy {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case config.ReserveThresholdConfirm:
		return ReserveConfirm
	case config.ReserveThresholdAuto:
		return ReserveAuto
	}
	return ReserveOff
}

func (p ReservePolicy) String() string {
	switch p {
	case ReserveConfirm:
		return config.ReserveThresholdConfirm
	case ReserveAuto:
		return config.ReserveThresholdAuto
	}
	return config.ReserveThresholdOff
}

// UsageLimitError is the typed "your quota is spent" verdict (M5 #25). A
// wire adapter that can tell a quota decision from a rate blip raises it
// directly; detectUsageLimit also classifies the common provider bodies,
// because this tree's adapters surface a plain *ai.HTTPError.
type UsageLimitError struct {
	Provider string
	Model    string
	Message  string
	// ResetAt is when the provider says the quota returns (zero = unknown).
	ResetAt time.Time
	// Banked marks a limit an adapter knows a stored provider reset can
	// lift early (the Codex auto-reset case): the RedeemReset seam is then
	// consulted even when the body carried no reset window.
	Banked bool
}

func (e *UsageLimitError) Error() string {
	if e == nil {
		return "usage limit"
	}
	msg := e.Message
	if msg == "" {
		msg = "usage limit reached"
	}
	if !e.ResetAt.IsZero() {
		msg += fmt.Sprintf(" (resets %s)", e.ResetAt.UTC().Format(time.RFC3339))
	}
	return fmt.Sprintf("usage limit for %s/%s: %s", e.Provider, e.Model, msg)
}

// usageLimitRe matches bodies that mean "the quota is spent", not "you are
// going too fast": a plain per-minute 429 deliberately stays in the
// transient backoff lane (the upstream classifier makes the same split).
var usageLimitRe = regexp.MustCompile(`(?i)usage limit|quota|insufficient[ _-]?credits?|out of credits|credit balance|exceeded your current|plan (limit|quota)|account policy|(daily|weekly|monthly) (limit|cap)`)

// resetHintRe pulls a reset window out of a body: "retry after 30s",
// "resets in 2h", or an absolute RFC3339 stamp.
var (
	resetAfterRe = regexp.MustCompile(`(?i)(?:retry[- ]after|resets? in|try again in)[:\s]+([0-9]+(?:\.[0-9]+)?\s*(?:ms|s|sec|secs|seconds|m|min|mins|minutes|h|hr|hrs|hours))`)
	resetAtRe    = regexp.MustCompile(`(?i)"?resets?_at"?[":\s]+"?([0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:]{8}(?:\.[0-9]+)?Z?)`)
)

// detectUsageLimit classifies err: the typed error first, then a 402/429/403
// HTTP body carrying a quota verdict.
func detectUsageLimit(err error, provider, model string) (*UsageLimitError, bool) {
	if err == nil {
		return nil, false
	}
	var typed *UsageLimitError
	if errors.As(err, &typed) {
		out := *typed
		if out.Provider == "" {
			out.Provider = provider
		}
		if out.Model == "" {
			out.Model = model
		}
		return &out, true
	}
	var he *ai.HTTPError
	if !errors.As(err, &he) {
		return nil, false
	}
	switch he.Status {
	case 402, 429, 403:
	default:
		return nil, false
	}
	if !usageLimitRe.MatchString(he.Body) {
		return nil, false
	}
	return &UsageLimitError{
		Provider: provider,
		Model:    model,
		Message:  ai.TruncateBody(he.Body, 200),
		ResetAt:  parseResetHint(he.Body),
	}, true
}

// parseResetHint reads the first reset window out of a body.
func parseResetHint(body string) time.Time {
	if m := resetAtRe.FindStringSubmatch(body); m != nil {
		if t, err := time.Parse(time.RFC3339, m[1]); err == nil {
			return t
		}
	}
	if m := resetAfterRe.FindStringSubmatch(body); m != nil {
		if d, ok := parseWindow(m[1]); ok {
			return time.Now().Add(d)
		}
	}
	return time.Time{}
}

// parseWindow accepts "30s", "30 s", "2 hours".
func parseWindow(raw string) (time.Duration, bool) {
	raw = strings.ToLower(strings.TrimSpace(raw))
	num, unit, ok := strings.Cut(raw, " ")
	if !ok {
		// No space: split the digits from the unit ("30s").
		i := 0
		for i < len(raw) && (raw[i] >= '0' && raw[i] <= '9' || raw[i] == '.') {
			i++
		}
		num, unit = raw[:i], raw[i:]
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	switch strings.TrimSpace(unit) {
	case "ms":
		return time.Duration(n * float64(time.Millisecond)), true
	case "s", "sec", "secs", "seconds":
		return time.Duration(n * float64(time.Second)), true
	case "m", "min", "mins", "minutes":
		return time.Duration(n * float64(time.Minute)), true
	case "h", "hr", "hrs", "hours":
		return time.Duration(n * float64(time.Hour)), true
	}
	return 0, false
}

// FallbackState is the live retry.fallbackChains machinery for one Agent
// (M5 #25). The zero value is usable but inert; Agent.Fallback nil disables
// every hook. Run is single-goroutine, so no lock is needed.
type FallbackState struct {
	// Role is the active model role ("" when the model was chosen
	// literally); it keys role chains and is reported in notices.
	Role string
	// Reserve is the pre-turn usage policy; ReserveFraction the
	// remaining-quota fraction that counts as near the limit.
	Reserve         ReservePolicy
	ReserveFraction float64
	// Revert is retry.fallbackRevertPolicy; Cooldown bounds how long a
	// fallback stays active before the primary is restored.
	Revert   string
	Cooldown time.Duration

	// Rotate rebuilds the active provider with the next models.yml
	// credential (multi-key). It returns the live provider and false once
	// the pool is exhausted. nil = no rotation.
	Rotate func(provider string) (ai.Provider, bool)
	// RedeemReset spends a banked provider reset (Codex auto-reset) and
	// reports whether the provider is usable again. Consulted before
	// rotating away from a provider. nil = nothing stored.
	RedeemReset func(provider string) bool
	// UsageProbe reports the active target's remaining quota as a 0..1
	// fraction (wired to a provider usage header). nil or ok=false means
	// unknown, and unknown never triggers a fallback.
	UsageProbe func(provider, model string) (remaining float64, ok bool)
	// Notify surfaces a notice (confirm mode, TUI). nil logs instead —
	// print mode and background agents have no console.
	Notify func(msg string)

	primary       ai.Provider
	primaryModel  string
	primaryWindow int
	switched      bool
	revertAt      time.Time
	cooldowns     map[string]time.Time // selector → suppressed until
	banked        map[string]time.Time // provider → quota reset
	rotations     int
	redeemed      bool // per-turn guard for a banked reset
}

// NewFallbackState builds the state from settings (nil settings → nil, the
// feature off).
func NewFallbackState(s *config.Settings, role string) *FallbackState {
	if s == nil {
		return nil
	}
	st := &FallbackState{
		Role:            role,
		Reserve:         ParseReservePolicy(s.ReservePolicy()),
		ReserveFraction: s.ReserveFraction(),
		Revert:          s.RevertPolicy(),
		Cooldown:        s.FallbackCooldownDuration(),
		cooldowns:       map[string]time.Time{},
		banked:          map[string]time.Time{},
	}
	return st
}

// ArmFallback installs the retry.fallbackChains state for the active model
// and returns it. The caller resolves the chain itself (ResolveFallbackChain
// + cmd/xdev's provider construction assigns a.Failovers first) and then
// sets the seams on the returned value:
//
//	st := ag.ArmFallback(settings, role)
//	st.Rotate = rotateCredential   // multi-key models.yml pool
//	st.UsageProbe = usageHeader    // provider usage header
//	st.Notify = app.notice         // TUI only
func (a *Agent) ArmFallback(s *config.Settings, role string) *FallbackState {
	if a == nil {
		return nil
	}
	st := NewFallbackState(s, role)
	if st == nil {
		return nil
	}
	st.primary, st.primaryModel = a.Provider, a.Model
	st.primaryWindow = a.Compaction.ContextWindow
	a.Fallback = st
	return st
}

// fallbackPreTurn is the per-turn entry point (called by
// oneTurnWithRecovery): it expires a fallback and applies the reserve
// policy before the turn is spent.
func (a *Agent) fallbackPreTurn() {
	st := a.Fallback
	if st == nil {
		return
	}
	st.redeemed = false
	now := time.Now()
	st.maybeRevert(a, now)
	st.maybeReserve(a, now)
}

// maybeReserve is the usage-aware pre-turn fallback
// (retry.reserveThreshold): a target reporting near-quota usage moves before
// the turn is spent on it. auto moves silently; confirm notifies first.
func (st *FallbackState) maybeReserve(a *Agent, now time.Time) {
	if st.Reserve == ReserveOff || st.UsageProbe == nil || a.Provider == nil {
		return
	}
	remaining, ok := st.UsageProbe(a.Provider.Name(), a.Model)
	if !ok || remaining > st.ReserveFraction {
		return
	}
	current := a.Provider.Name() + "/" + a.Model
	if st.Reserve == ReserveConfirm {
		st.notify(fmt.Sprintf("%s is near its quota (%.0f%% left) — falling back", current, remaining*100))
	}
	if !a.usageMove("reserve", now) {
		logx.Errorf("reserve fallback: %s is near its quota but no target is usable", current)
	}
}

// recoverUsageLimit handles a usage-limit failure: record the reset window,
// spend a banked reset when one is available, rotate to a sibling
// credential, or step the chain. false means the caller should treat the
// error normally (the ladder exhausted every target — never loop).
func (a *Agent) recoverUsageLimit(err error) bool {
	st := a.Fallback
	if st == nil || a.Provider == nil {
		return false
	}
	ul, ok := detectUsageLimit(err, a.Provider.Name(), a.Model)
	if !ok {
		return false
	}
	now := time.Now()
	st.noteLimit(ul, now)
	logx.Errorf("usage limit on %s/%s: %s", ul.Provider, ul.Model, ul.Message)
	if !a.usageMove("usage-limit", now) {
		st.notify(fmt.Sprintf("every fallback target is exhausted; surfacing the usage limit on %s/%s", ul.Provider, ul.Model))
		return false
	}
	return true
}

// usageMove leaves a target that cannot serve: a banked reset first (the
// provider is usable again), then a sibling credential (a quota is
// account-scoped, so rotating keeps the model), then the next chain entry.
func (a *Agent) usageMove(reason string, now time.Time) bool {
	st := a.Fallback
	if st == nil {
		return false
	}
	if st.redeemBanked(a.Provider.Name(), now) {
		return true
	}
	if st.rotate(a) {
		return true
	}
	if nxt := a.nextFailoverTarget(); nxt > 0 {
		a.switchTarget(nxt, reason)
		return true
	}
	return false
}

// rotate swaps in the provider's next credential, keeping the model: a
// spent quota is per account, so a sibling account can serve the same
// request. Returns true when the provider object changed.
func (st *FallbackState) rotate(a *Agent) bool {
	if st.Rotate == nil || st.rotations >= maxRotationsPerProvider || a.Provider == nil {
		return false
	}
	p, ok := st.Rotate(a.Provider.Name())
	if !ok || p == nil {
		return false
	}
	st.rotations++
	a.Provider = p
	if a.curTarget > 0 && a.curTarget <= len(a.Failovers) {
		a.Failovers[a.curTarget-1].Provider = p
	}
	st.notify(fmt.Sprintf("rotated to the next %s credential (same model %s)", p.Name(), a.Model))
	return true
}

// redeemBanked spends a stored provider reset when one is on record for the
// provider — a future window, or the zero-time flag an adapter raises when
// it knows a reset exists but the body carried no window (Codex auto-reset).
// At most once per turn.
func (st *FallbackState) redeemBanked(provider string, now time.Time) bool {
	if st.redeemed || st.RedeemReset == nil {
		return false
	}
	until, ok := st.banked[provider]
	if !ok || (!until.IsZero() && !until.After(now)) {
		return false
	}
	if !st.RedeemReset(provider) {
		return false
	}
	st.redeemed = true
	delete(st.banked, provider)
	when := "now"
	if !until.IsZero() {
		when = until.UTC().Format(time.RFC3339)
	}
	st.notify(fmt.Sprintf("redeemed the banked %s reset; provider usable again (%s)", provider, when))
	return true
}

// noteLimit records a usage-limit verdict: the provider's reset window
// (honored by suppressed) and the selector cooldown. An adapter-reported
// banked reset with no window is recorded as the zero time so redemption
// is still offered, without suppressing the provider.
func (st *FallbackState) noteLimit(ul *UsageLimitError, now time.Time) {
	if st.banked == nil {
		st.banked = map[string]time.Time{}
	}
	if !ul.ResetAt.IsZero() && ul.ResetAt.After(now) {
		st.banked[ul.Provider] = ul.ResetAt
	} else if ul.Banked {
		st.banked[ul.Provider] = time.Time{}
	}
	sel := ul.Provider + "/" + ul.Model
	if st.cooldowns == nil {
		st.cooldowns = map[string]time.Time{}
	}
	until := ul.ResetAt
	if !until.After(now) {
		until = now.Add(st.cooldown())
	}
	st.cooldowns[sel] = until
}

// onSwitch records a fallback: the selector that just failed is pinned for
// its cooldown window (so the ladder does not bounce straight back), and
// the primary's restoration deadline starts.
func (st *FallbackState) onSwitch(prev, reason string, now time.Time) {
	if st == nil || !fallbackReasons[reason] {
		return
	}
	if st.cooldowns == nil {
		st.cooldowns = map[string]time.Time{}
	}
	if prev != "" {
		pin := st.cooldown()
		if reason == "recovery" {
			pin = transientPin
		}
		if until, banked := st.banked[strings.SplitN(prev, "/", 2)[0]]; banked {
			pin = until.Sub(now) // the provider's own reset window wins
		}
		if pin > 0 {
			st.cooldowns[prev] = now.Add(pin)
		}
	}
	st.switched = true
	st.revertAt = now.Add(st.cooldown())
	st.rotations = 0
}

// fallbackReasons are the switch reasons that install a fallback. A prewalk
// or plan-yolo handoff is a deliberate model change, not a fallback, and
// must never be auto-reverted.
var fallbackReasons = map[string]bool{"recovery": true, "usage-limit": true, "reserve": true}

// suppressed reports whether a selector is inside a cooldown window or its
// provider has a banked reset pending: the ladder must not select a target
// the provider just refused.
func (st *FallbackState) suppressed(sel string, now time.Time) bool {
	if st == nil {
		return false
	}
	if until, ok := st.cooldowns[sel]; ok && until.After(now) {
		return true
	}
	provider := sel
	if i := strings.Index(sel, "/"); i >= 0 {
		provider = sel[:i]
	}
	if until, ok := st.banked[provider]; ok && until.After(now) {
		return true
	}
	return false
}

// maybeRevert restores the primary model once the fallback cooldown has
// expired (retry.fallbackRevertPolicy: cooldown-expiry) and the primary is
// no longer suppressed. The restore is itself a model_change entry, so the
// transcript shows the round trip.
func (st *FallbackState) maybeRevert(a *Agent, now time.Time) bool {
	if st == nil || !st.switched || st.primary == nil {
		return false
	}
	if st.Revert == config.RevertNever || now.Before(st.revertAt) {
		return false
	}
	sel := st.primary.Name() + "/" + st.primaryModel
	if st.suppressed(sel, now) {
		return false
	}
	a.curTarget = 0
	a.Provider = st.primary
	a.Model = st.primaryModel
	a.Compaction.ContextWindow = st.primaryWindow
	st.switched = false
	logx.Errorf("fallback cooldown expired: restored %s", sel)
	a.recordModelChange(sel, false)
	return true
}

// cooldown is the effective fallback cooldown (0 → the config default).
func (st *FallbackState) cooldown() time.Duration {
	if st.Cooldown > 0 {
		return st.Cooldown
	}
	return config.DefaultFallbackCooldown
}

// notify surfaces a message: the caller's Notify in confirm mode, the log
// otherwise (print mode and background agents have no console).
func (st *FallbackState) notify(msg string) {
	if st != nil && st.Reserve == ReserveConfirm && st.Notify != nil {
		st.Notify(msg)
		return
	}
	logx.Errorf("%s", msg)
}

// recordModelChange mirrors a model switch into the session store. The
// fallback flag is the wire's own "this model is a fallback" attribution.
func (a *Agent) recordModelChange(sel string, fallback bool) {
	if a.Store == nil {
		return
	}
	_ = a.Store.Append(&session.ModelChangeEntry{Model: sel, ResolvedModelIsFallback: fallback})
}
