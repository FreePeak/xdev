package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	ownerA = "aaaa1111-1111-1111-1111-111111111111"
	ownerB = "bbbb2222-2222-2222-2222-222222222222"
)

// TestMailboxRoundTripAcrossInstances proves persistence: a second Mailbox
// instance (as another session would open) sees messages sent to its inbox,
// and MarkRead survives a re-read.
func TestMailboxRoundTripAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	a := NewMailbox(dir)
	b := NewMailbox(dir)
	a.SetOwner(ownerA)
	b.SetOwner(ownerB)

	if _, err := a.SendMessage(ownerB, "build done", "green"); err != nil {
		t.Fatalf("send: %v", err)
	}

	inbox, err := b.Inbox()
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	if len(inbox) != 1 {
		t.Fatalf("want 1 message, got %d", len(inbox))
	}
	msg := inbox[0]
	if msg.From != mailID(ownerA) {
		t.Errorf("from = %q, want %q", msg.From, mailID(ownerA))
	}
	if msg.Subject != "build done" || msg.Body != "green" {
		t.Errorf("subject/body = %q/%q", msg.Subject, msg.Body)
	}

	unread, err := b.Unread()
	if err != nil || len(unread) != 1 {
		t.Fatalf("unread = %v, %v", unread, err)
	}
	if err := b.MarkRead(msg.ID); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	if unread, _ := b.Unread(); len(unread) != 0 {
		t.Errorf("unread after mark = %d, want 0", len(unread))
	}
	if inbox, _ := b.Inbox(); len(inbox) != 1 {
		t.Errorf("inbox after mark = %d, want 1 (read messages are kept)", len(inbox))
	}
}

// TestMailboxPollerDelivery covers first-sweep delivery of a pre-existing
// message, callback + Wait delivery of a message arriving after Start, and
// idempotent Stop.
func TestMailboxPollerDelivery(t *testing.T) {
	dir := t.TempDir()
	a := NewMailbox(dir)
	b := NewMailbox(dir)
	a.SetOwner(ownerA)
	b.SetOwner(ownerB)

	if _, err := a.SendMessage(ownerB, "before start", "early"); err != nil {
		t.Fatalf("send: %v", err)
	}

	poller := NewInboxPoller(b)
	poller.Interval = 10 * time.Millisecond
	gotCallback := make(chan Message, 8)
	poller.OnMessage = func(m Message) bool { gotCallback <- m; return true }
	poller.Start()
	defer poller.Stop()

	msg, ok := poller.Wait(2 * time.Second)
	if !ok || msg.Subject != "before start" {
		t.Fatalf("wait = %q, %v; want first-sweep delivery of pre-existing message", msg.Subject, ok)
	}
	if _, err := a.SendMessage(ownerB, "after start", "late"); err != nil {
		t.Fatalf("send: %v", err)
	}
	msg, ok = poller.Wait(2 * time.Second)
	if !ok || msg.Subject != "after start" {
		t.Fatalf("wait = %q, %v; want delivery of message sent after Start", msg.Subject, ok)
	}
	select {
	case m := <-gotCallback:
		if m.Subject != "after start" && m.Subject != "before start" {
			t.Errorf("callback got %q", m.Subject)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnMessage callback never fired")
	}

	poller.Stop() // second stop must not panic
	if unread, _ := b.Unread(); len(unread) != 0 {
		t.Errorf("poller left %d unread; delivered messages should be marked read", len(unread))
	}
}

// TestMailboxCapEviction proves the oldest-drop cap.
func TestMailboxCapEviction(t *testing.T) {
	defer func(old int) { mailboxCap = old }(mailboxCap)
	mailboxCap = 50

	dir := t.TempDir()
	m := NewMailbox(dir)
	m.SetOwner(ownerA)

	var firstID, lastID string
	for i := 0; i < 55; i++ {
		msg, err := m.SendMessage(ownerA, fmt.Sprintf("m%02d", i), "x")
		if err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		if i == 0 {
			firstID = msg.ID
		}
		lastID = msg.ID
	}

	inbox, err := m.Inbox()
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	if len(inbox) != 50 {
		t.Fatalf("inbox = %d, want 50", len(inbox))
	}
	for _, msg := range inbox {
		if msg.ID == firstID {
			t.Error("oldest message survived eviction")
		}
	}
	if inbox[len(inbox)-1].ID != lastID {
		t.Error("newest message evicted")
	}
}

// TestMailboxIdentity covers RegisterIdentity binding, Resolve fallback, and
// that a name-addressed send lands in the bound session's inbox.
func TestMailboxIdentity(t *testing.T) {
	dir := t.TempDir()
	m := NewMailbox(dir)
	m.SetOwner(ownerA)

	if err := m.RegisterIdentity("worker", ownerB); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "agents", "worker.json")); err != nil {
		t.Fatalf("identity file: %v", err)
	}
	if got := m.Resolve("worker"); got != ownerB {
		t.Errorf("resolve(worker) = %q, want %q", got, ownerB)
	}
	if got := m.Resolve("nobody-here"); got != "nobody-here" {
		t.Errorf("resolve unknown = %q, want passthrough", got)
	}

	msg, err := m.SendMessage("worker", "task", "do it")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if msg.To != mailID(ownerB) {
		t.Errorf("to = %q, want resolved short id %q", msg.To, mailID(ownerB))
	}
	worker := NewMailbox(dir)
	worker.SetOwner(ownerB)
	inbox, err := worker.Inbox()
	if err != nil || len(inbox) != 1 || inbox[0].Subject != "task" {
		t.Fatalf("bound-session inbox = %v, %v; want the name-addressed message", inbox, err)
	}
}

// TestInboxToolOwnInboxOnly proves the tool reads only the bound session's
// inbox and marks listed messages read.
func TestInboxToolOwnInboxOnly(t *testing.T) {
	dir := t.TempDir()
	ma := NewMailbox(dir)
	mb := NewMailbox(dir)
	ma.SetOwner(ownerA)
	mb.SetOwner(ownerB)

	toolA := &InboxTool{Mailbox: ma}

	res, _ := toolA.Execute(context.Background(), json.RawMessage(`{}`))
	if res.IsError || res.Text != "inbox: empty" {
		t.Fatalf("empty inbox result = %q (err=%v)", res.Text, res.IsError)
	}

	if _, err := mb.SendMessage(ownerA, "for a", "hello a"); err != nil {
		t.Fatalf("send to a: %v", err)
	}
	if _, err := ma.SendMessage(ownerB, "for b", "hello b"); err != nil {
		t.Fatalf("send to b: %v", err)
	}

	res, _ = toolA.Execute(context.Background(), json.RawMessage(`{}`))
	if res.IsError {
		t.Fatalf("tool error: %s", res.Text)
	}
	if !strings.Contains(res.Text, "for a") || !strings.Contains(res.Text, "hello a") {
		t.Errorf("missing own message in:\n%s", res.Text)
	}
	if strings.Contains(res.Text, "for b") || strings.Contains(res.Text, "hello b") {
		t.Errorf("leaked another session's message:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, "[new]") {
		t.Errorf("first listing should flag the unread message:\n%s", res.Text)
	}
	res, _ = toolA.Execute(context.Background(), json.RawMessage(`{}`))
	if strings.Contains(res.Text, "[new]") || !strings.Contains(res.Text, "[read]") {
		t.Errorf("second listing should show the message read:\n%s", res.Text)
	}
	if unread, _ := ma.Unread(); len(unread) != 0 {
		t.Errorf("tool left %d unread", len(unread))
	}
}

// TestSendMessageToolErrors covers the unbound-owner and missing-argument
// error paths through the tool surface.
func TestSendMessageToolErrors(t *testing.T) {
	unbound := &SendMessageTool{Mailbox: NewMailbox(t.TempDir())}
	res, _ := unbound.Execute(context.Background(), json.RawMessage(`{"to":"bbbb2222","body":"x"}`))
	if !res.IsError {
		t.Error("unbound owner should be an error result")
	}

	m := unbound.Mailbox
	m.SetOwner(ownerA)
	st := &SendMessageTool{Mailbox: m}
	res, _ = st.Execute(context.Background(), json.RawMessage(`{"to":"bbbb2222"}`))
	if !res.IsError || !strings.Contains(res.Text, "to and body are required") {
		t.Errorf("missing body: %q (err=%v)", res.Text, res.IsError)
	}
	res, _ = st.Execute(context.Background(), json.RawMessage(`{"to":"bbbb2222","body":"ok"}`))
	if res.IsError {
		t.Fatalf("send: %s", res.Text)
	}
	if !strings.Contains(res.Text, "bbbb2222") {
		t.Errorf("confirmation should name the recipient: %q", res.Text)
	}
}

// TestConcurrentAppendsLineAtomic proves concurrent senders to one inbox all
// land (O_APPEND line atomicity).
func TestConcurrentAppendsLineAtomic(t *testing.T) {
	dir := t.TempDir()
	m := NewMailbox(dir)
	m.SetOwner(ownerA)

	const goroutines, perG = 4, 25
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			mb := NewMailbox(dir)
			mb.SetOwner(fmt.Sprintf("send%02d-0000-0000-0000-000000000000", g))
			for i := 0; i < perG; i++ {
				if _, err := mb.SendMessage(ownerA, "concurrent", "x"); err != nil {
					t.Errorf("send: %v", err)
				}
			}
		}(g)
	}
	wg.Wait()

	inbox, err := m.Inbox()
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	if len(inbox) != goroutines*perG {
		t.Errorf("inbox = %d, want %d", len(inbox), goroutines*perG)
	}
}

// A message that reaches no live sink stays UNREAD (#90): before this, the
// poller marked read unconditionally, so a session that polled while nothing
// was listening consumed its own mailbox and the `inbox` tool showed nothing.
func TestInboxPollerLeavesUndeliveredUnread(t *testing.T) {
	dir := t.TempDir()
	mb := NewMailbox(dir)
	mb.SetOwner("recipient")
	sender := NewMailbox(dir)
	sender.SetOwner("sender")
	if _, err := sender.SendMessage("recipient", "peer note", "body"); err != nil {
		t.Fatal(err)
	}

	p := NewInboxPoller(mb)
	p.Interval = 10 * time.Millisecond
	p.OnMessage = func(Message) bool { return false } // declining sink
	p.Start()
	time.Sleep(120 * time.Millisecond)
	p.Stop()

	unread, err := mb.Unread()
	if err != nil {
		t.Fatal(err)
	}
	if len(unread) != 1 || unread[0].Subject != "peer note" {
		t.Fatalf("a declined message must stay unread, got %+v", unread)
	}

	// Accepting it now consumes it exactly once.
	p2 := NewInboxPoller(mb)
	p2.Interval = 10 * time.Millisecond
	var got []Message
	p2.OnMessage = func(m Message) bool { got = append(got, m); return true }
	p2.Start()
	deadline := time.Now().Add(2 * time.Second)
	for len(got) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	p2.Stop()
	if len(got) != 1 {
		t.Fatalf("accepted delivery = %d, want 1", len(got))
	}
	if left, _ := mb.Unread(); len(left) != 0 {
		t.Fatalf("an accepted message must be marked read: %+v", left)
	}
}
