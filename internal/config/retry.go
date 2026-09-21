package config

import (
	"fmt"
	"strings"
	"time"
)

// Retry fallback-chain settings (M5 #25, docs/research/omp-context-resilience
// §1.6-1.7). The `retry` group is deliberately small: the chain TABLE plus
// three scalars. Everything the agent needs at runtime is derived from it by
// methods below, so a change here cannot drift from the behaviour.

// retry.reserveThreshold values: what to do when the active target is near
// its quota/limit.
const (
	// ReserveThresholdOff is the shipped default: usage is never consulted.
	ReserveThresholdOff = "off"
	// ReserveThresholdAuto switches to the next chain entry silently.
	ReserveThresholdAuto = "auto"
	// ReserveThresholdConfirm surfaces a notice before switching.
	ReserveThresholdConfirm = "confirm"
)

// retry.fallbackRevertPolicy values.
const (
	// RevertCooldownExpiry restores the primary model once the fallback
	// cooldown has expired (the shipped default).
	RevertCooldownExpiry = "cooldown-expiry"
	// RevertNever keeps the fallback for the rest of the session.
	RevertNever = "never"
)

// DefaultFallbackCooldown bounds how long a fallback stays active before the
// primary is restored; DefaultReservePct is the remaining-quota fraction that
// counts as "near the limit".
const (
	DefaultFallbackCooldown = 5 * time.Minute
	DefaultReservePct       = 10
)

// RetrySettings is the `retry` group. FallbackChains keys are two-way:
//
//	onegw/free                    exact model selector
//	onegw/*  openrouter/google/*  provider wildcard
//
// and the entries are model refs, accepted with the same wildcards.
type RetrySettings struct {
	// FallbackChains maps a chain key to its ordered model refs. An
	// empty/absent map leaves the models.yml outage chain in charge.
	FallbackChains map[string][]string `yaml:"fallbackChains"`
	// ReserveThreshold is off | auto | confirm (empty → off).
	ReserveThreshold string `yaml:"reserveThreshold"`
	// ReservePct is the remaining-quota percentage below which a target
	// counts as near its limit (0 → DefaultReservePct).
	ReservePct float64 `yaml:"reservePct"`
	// FallbackRevertPolicy is cooldown-expiry | never (empty → the
	// default, cooldown-expiry).
	FallbackRevertPolicy string `yaml:"fallbackRevertPolicy"`
	// FallbackCooldown is a Go duration ("5m") bounding how long a
	// fallback stays active before the primary is restored.
	FallbackCooldown string `yaml:"fallbackCooldown"`
	// RetryAllErrors makes every error class — including
	// empty turns (ErrEmptyTurn, "the model produced no answer")
	// — retryable: Run rebuilds context from history and
	// re-runs the ladder instead of ending the session, so a
	// transient upstream stall never looks like a silent death
	// (#331 follow-up: keep going until the goal is done).
	// Off by default — an empty turn is usually the model being
	// done, and retrying it forever burns tokens on a hard stop.
	// -retry-all-errors forces it on for one run.
	RetryAllErrors *bool `yaml:"retryAllErrors"`
	// Infinite is "always retry": once the whole fallback chain has
	// drained, the recovery ladder keeps re-running it instead of
	// surfacing the error, so an outage (a gateway that answers
	// `connection refused`) is survived however long it lasts. Off by
	// default: a hard failure misclassified as transient would then
	// spin forever. -retry-forever forces it on for one run.
	Infinite *bool `yaml:"infinite"`
}

// RetryConfig returns the retry group (nil-safe: a missing layer means the
// shipped defaults).
func (s *Settings) RetryConfig() RetrySettings {
	if s == nil {
		return RetrySettings{}
	}
	return s.Retry
}

// InfiniteRetry reports whether retry.infinite is on. It is default-on:
// an outage may not end a run. An explicit false bounds the ladder again
// (a one-way merge can never express "I want the bounded ladder").
func (s *Settings) InfiniteRetry() bool {
	if s == nil || s.Retry.Infinite == nil {
		return true
	}
	return *s.Retry.Infinite
}

// RetryAllErrors reports whether retry.retryAllErrors is on. It is
// default-on: an empty turn is usually the model being done, and
// retrying it forever burns tokens on a hard stop. An explicit
// false bounds the ladder again (a one-way merge can never
// express "I want the bounded ladder").
func (s *Settings) RetryAllErrors() bool {
	if s == nil || s.Retry.RetryAllErrors == nil {
		return true
	}
	return *s.Retry.RetryAllErrors
}

// ReservePolicy returns the normalized retry.reserveThreshold value.
func (s *Settings) ReservePolicy() string {
	v := strings.ToLower(strings.TrimSpace(s.RetryConfig().ReserveThreshold))
	switch v {
	case ReserveThresholdAuto, ReserveThresholdConfirm:
		return v
	}
	return ReserveThresholdOff
}

// RevertPolicy returns the normalized retry.fallbackRevertPolicy value.
func (s *Settings) RevertPolicy() string {
	if strings.ToLower(strings.TrimSpace(s.RetryConfig().FallbackRevertPolicy)) == RevertNever {
		return RevertNever
	}
	return RevertCooldownExpiry
}

// FallbackCooldownDuration parses retry.fallbackCooldown (0 → the default).
func (s *Settings) FallbackCooldownDuration() time.Duration {
	raw := strings.TrimSpace(s.RetryConfig().FallbackCooldown)
	if raw == "" {
		return DefaultFallbackCooldown
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return DefaultFallbackCooldown
	}
	return d
}

// ReserveFraction is the remaining-quota fraction (0..1) that counts as
// "near the limit" (0 → the default 10%).
func (s *Settings) ReserveFraction() float64 {
	pct := s.RetryConfig().ReservePct
	if pct <= 0 || pct > 100 {
		pct = DefaultReservePct
	}
	return pct / 100
}

// mergeRetry applies one later layer's retry group over the receiver. Maps
// deep-merge per key (a layer may extend a chain table without restating it),
// scalars replace. Called from merge().
func (s *Settings) mergeRetry(layer *Settings) error {
	if layer == nil {
		return nil
	}
	for k, v := range layer.Retry.FallbackChains {
		k = strings.TrimSpace(k)
		if k == "" {
			return fmt.Errorf("retry.fallbackChains: empty chain key")
		}
		if s.Retry.FallbackChains == nil {
			s.Retry.FallbackChains = map[string][]string{}
		}
		s.Retry.FallbackChains[k] = append([]string(nil), v...)
	}
	if layer.Retry.ReserveThreshold != "" {
		if err := validReserveThreshold(layer.Retry.ReserveThreshold); err != nil {
			return err
		}
		s.Retry.ReserveThreshold = layer.Retry.ReserveThreshold
	}
	if layer.Retry.ReservePct != 0 {
		if layer.Retry.ReservePct < 0 || layer.Retry.ReservePct > 100 {
			return fmt.Errorf("retry.reservePct must be 0-100, got %v", layer.Retry.ReservePct)
		}
		s.Retry.ReservePct = layer.Retry.ReservePct
	}
	if layer.Retry.FallbackRevertPolicy != "" {
		if err := validRevertPolicy(layer.Retry.FallbackRevertPolicy); err != nil {
			return err
		}
		s.Retry.FallbackRevertPolicy = layer.Retry.FallbackRevertPolicy
	}
	if layer.Retry.FallbackCooldown != "" {
		if _, err := time.ParseDuration(layer.Retry.FallbackCooldown); err != nil {
			return fmt.Errorf("retry.fallbackCooldown %q: %w", layer.Retry.FallbackCooldown, err)
		}
		s.Retry.FallbackCooldown = layer.Retry.FallbackCooldown
	}
	// One-way: a later layer may only turn these on,
	// never off — the shipped defaults stay the bounded ladder.
	if layer.Retry.Infinite != nil && *layer.Retry.Infinite {
		s.Retry.Infinite = layer.Retry.Infinite
	}
	if layer.Retry.RetryAllErrors != nil && *layer.Retry.RetryAllErrors {
		s.Retry.RetryAllErrors = layer.Retry.RetryAllErrors
	}
	return validateRetryChains(s.Retry.FallbackChains)
}

// validReserveThreshold rejects a misspelled policy rather than silently
// disabling the feature.
func validReserveThreshold(v string) error {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case ReserveThresholdOff, ReserveThresholdAuto, ReserveThresholdConfirm:
		return nil
	}
	return fmt.Errorf("unknown retry.reserveThreshold %q (want off|auto|confirm)", v)
}

// validRevertPolicy rejects a misspelled revert policy.
func validRevertPolicy(v string) error {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case RevertCooldownExpiry, RevertNever:
		return nil
	}
	return fmt.Errorf("unknown retry.fallbackRevertPolicy %q (want cooldown-expiry|never)", v)
}

// validateRetryChains rejects a chain that cannot resolve: an empty entry
// would silently shorten the ladder a user explicitly wrote out.
func validateRetryChains(chains map[string][]string) error {
	for k, entries := range chains {
		for i, e := range entries {
			if strings.TrimSpace(e) == "" {
				return fmt.Errorf("retry.fallbackChains.%s: entry %d is empty", k, i+1)
			}
		}
	}
	return nil
}
