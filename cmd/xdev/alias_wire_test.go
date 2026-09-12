package main

import (
	"testing"

	"github.com/FreePeak/xdev/internal/agent"
	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/session"
)

// TestAliasWiredToMailboxIdentity is the cmd seam for --alias (M14 #63):
// wireTaskParent registers the configured name as the session's agent
// identity, so a peer session resolves that name to this session's inbox.
func TestAliasWiredToMailboxIdentity(t *testing.T) {
	t.Setenv("XDEV_AGENT_DIR", t.TempDir())
	t.Cleanup(func() { _ = config.SetAlias("") })

	if err := config.SetAlias("worker"); err != nil {
		t.Fatal(err)
	}
	reg := newToolRegistry(t.TempDir(), nil, "p", "m", nil, nil, nil)
	raw, ok := reg.Get(agent.InboxToolName)
	if !ok {
		t.Fatal("inbox tool not registered")
	}
	it, ok := raw.(*agent.InboxTool)
	if !ok {
		t.Fatalf("inbox tool is %T, want *agent.InboxTool", raw)
	}
	mailbox := it.Mailbox
	store := session.OpenMem("/proj", "alias seam")
	t.Cleanup(func() { _ = store.Close() })

	wireTaskParent(reg, store)

	if got := mailbox.Resolve("worker"); got != store.ID() {
		t.Fatalf("resolve(worker) = %q, want session id %q", got, store.ID())
	}
	if _, err := mailbox.SendMessage("worker", "ping", "body"); err != nil {
		t.Fatalf("send by alias: %v", err)
	}
	inbox, err := mailbox.Inbox()
	if err != nil || len(inbox) != 1 || inbox[0].Subject != "ping" {
		t.Fatalf("inbox = %+v, %v; want the one aliased message", inbox, err)
	}

	// A session switch (the /resume path) re-registers under the new id.
	next := session.OpenMem("/proj", "second session")
	t.Cleanup(func() { _ = next.Close() })
	wireTaskParent(reg, next)
	if got := mailbox.Resolve("worker"); got != next.ID() {
		t.Fatalf("after rebind resolve(worker) = %q, want %q", got, next.ID())
	}
}
