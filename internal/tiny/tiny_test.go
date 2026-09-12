package tiny

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// counting is a Completer that records calls, standing in for the API
// completer the memory pipeline is wired with today.
type counting struct {
	calls int
	out   string
	err   error
}

func (c *counting) fn(_ context.Context, _ string) (string, error) {
	c.calls++
	return c.out, c.err
}

// Acceptance: the seam's default keeps the API path unchanged.
func TestSelectDefaultsToAPI(t *testing.T) {
	for _, env := range []string{"", "off", "0", "false", "no"} {
		t.Setenv(EnvLocal, env)
		api := &counting{out: "api reply"}
		got, err := Select(TaskMemoryExtract, api.fn)
		if err != nil {
			t.Fatalf("env %q: Select: %v", env, err)
		}
		if got == nil {
			t.Fatalf("env %q: nil completer", env)
		}
		out, err := got(context.Background(), "prompt")
		if err != nil || out != "api reply" {
			t.Fatalf("env %q: got (%q, %v), want (\"api reply\", nil)", env, out, err)
		}
		if api.calls != 1 {
			t.Fatalf("env %q: api calls = %d, want 1 (the default path is the API)", env, api.calls)
		}
	}
}

// Acceptance: an explicit opt-in with no runtime fails loudly AND does not
// hand back the API completer (no silent fallback to, or away from, local).
func TestSelectLocalRequestedIsNotBuilt(t *testing.T) {
	for _, env := range []string{"on", "1", "true", "yes", "ON"} {
		t.Setenv(EnvLocal, env)
		api := &counting{out: "api reply"}
		got, err := Select(TaskMemoryConsolidate, api.fn)
		if got != nil {
			t.Fatalf("env %q: completer = %v, want nil", env, got)
		}
		if !errors.Is(err, ErrNotBuilt) {
			t.Fatalf("env %q: err = %v, want ErrNotBuilt", env, err)
		}
		if !strings.Contains(err.Error(), string(TaskMemoryConsolidate)) {
			t.Fatalf("env %q: err %v does not name the task", env, err)
		}
		if api.calls != 0 {
			t.Fatalf("env %q: api called %d times despite the opt-in", env, api.calls)
		}
	}
}

// Acceptance: the stub reports unavailability explicitly, contract-complete.
func TestStubReportsUnavailability(t *testing.T) {
	b := Backend()
	if b == nil {
		t.Fatal("Backend() = nil, want the stub")
	}
	if b.Available() {
		t.Fatal("stub reports Available; the shipped artifact links no runtime")
	}
	if b.Name() == "" {
		t.Fatal("stub has an empty Name; messages would be anonymous")
	}
	if _, err := b.Complete(context.Background(), "prompt"); !errors.Is(err, ErrNotBuilt) {
		t.Fatalf("stub Complete err = %v, want ErrNotBuilt", err)
	}
}

// A typo must not silently mean "off" (that would keep tiny work on the API
// while the user believes it is on-device).
func TestRequestedRejectsUnknownValues(t *testing.T) {
	t.Setenv(EnvLocal, "maybe")
	if _, err := Requested(); err == nil {
		t.Fatal("Requested(maybe) = nil error, want a loud parse failure")
	}
	if _, err := Select(TaskTitle, (&counting{}).fn); err == nil {
		t.Fatal("Select with a bad env = nil error, want the parse failure")
	}
}

// The default env-less build has one honest answer: no local backend was
// compiled in, so nothing anywhere can silently pick one up.
func TestNoLocalBackendCompiledIn(t *testing.T) {
	t.Setenv(EnvLocal, "")
	api := &counting{out: "x"}
	if _, err := Select(TaskTitle, api.fn); err != nil {
		t.Fatalf("default Select: %v", err)
	}
	if b := Backend(); b.Available() {
		t.Fatalf("default build has an available local backend %q: the CGO-free artifact must not", b.Name())
	}
}
