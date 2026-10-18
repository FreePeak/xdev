package opencode

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLiveReadOnly is opt-in: XDEV_OPENCODE_LIVE=1 go test ./internal/opencode/
//
// It is deliberately read-only. Discovery plus one session list creates no
// session, sends no prompt, and changes nothing on the user's server — it only
// checks that the argv this package builds is argv the installed opencode
// actually accepts. A fake-backed test cannot catch a wrong operation id or a
// renamed flag; only the real binary can.
func TestLiveReadOnly(t *testing.T) {
	if os.Getenv("XDEV_OPENCODE_LIVE") != "1" {
		t.Skip("set XDEV_OPENCODE_LIVE=1 to run against the installed opencode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c := New()
	ep, err := c.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	t.Logf("endpoint %s (password %d chars)", ep.URL, len(ep.Password))
	if ep.Password == "" {
		t.Fatal("empty password")
	}

	rows, err := c.ListSessions(ctx, "")
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	t.Logf("session.list returned %d rows", len(rows))
	for i, r := range rows {
		if i >= 3 {
			break
		}
		t.Logf("  %s agent=%s model=%s/%s cost=%.6f in=%d out=%d title=%q dir=%s",
			r.ID, r.Agent, r.Model.ProviderID, r.Model.ID, r.Cost,
			r.Tokens.Input, r.Tokens.Output, r.Title, r.Location.Directory)
	}

	// GetSession on the first row proves session.get and its sessionID param.
	if len(rows) > 0 {
		got, err := c.GetSession(ctx, rows[0].ID)
		if err != nil {
			t.Fatalf("GetSession(%s): %v", rows[0].ID, err)
		}
		if got.ID != rows[0].ID {
			t.Errorf("id mismatch: %q vs %q", got.ID, rows[0].ID)
		}
	}
}
