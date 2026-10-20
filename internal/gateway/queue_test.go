package gateway

import (
	"sync"
	"testing"
	"time"
)

// waitFor polls cond until it holds or the budget runs out. The queue's
// runner is a goroutine, so every assertion about it is a wait, not a sleep.
func waitFor(t *testing.T, budget time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
}

// TestQueueKeepsAChatInOrder: two messages for one chat must run in the order
// they arrived. The session is a conversation, and a reversed pair is a
// conversation the model reads backwards.
func TestQueueKeepsAChatInOrder(t *testing.T) {
	var mu sync.Mutex
	var got []string
	q := newTurnQueue(func(chat int64, text string) {
		mu.Lock()
		got = append(got, text)
		mu.Unlock()
	})
	q.push(1, "first")
	q.push(1, "second")
	q.push(1, "third")

	if !waitFor(t, 3*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 3
	}) {
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("queue ran %v, want all three turns", got)
	}
	mu.Lock()
	defer mu.Unlock()
	for i, want := range []string{"first", "second", "third"} {
		if got[i] != want {
			t.Fatalf("turn %d = %q, want %q (order is the conversation)", i, got[i], want)
		}
	}
}

// TestQueueDoesNotLetOneChatBlockAnother: a chat whose turn never returns
// must not stop another chat from being answered. This is the wedge: one
// wedged worker used to freeze the whole bridge.
func TestQueueDoesNotLetOneChatBlockAnother(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	var ran []int64
	q := newTurnQueue(func(chat int64, text string) {
		if chat == 1 {
			<-release // the wedged turn
			return
		}
		mu.Lock()
		ran = append(ran, chat)
		mu.Unlock()
	})
	q.push(1, "wedged")
	q.push(2, "healthy")

	if !waitFor(t, 3*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(ran) == 1
	}) {
		t.Fatal("the healthy chat never ran while another chat was wedged")
	}
	close(release)
}

// TestQueueRunsEveryQueuedTurn: a burst must not be swallowed. The queue is
// unbounded on purpose — every message the user sent deserves an answer.
func TestQueueRunsEveryQueuedTurn(t *testing.T) {
	var mu sync.Mutex
	n := 0
	q := newTurnQueue(func(chat int64, text string) {
		mu.Lock()
		n++
		mu.Unlock()
	})
	for i := 0; i < 25; i++ {
		q.push(7, "message")
	}
	if !waitFor(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return n == 25
	}) {
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("ran %d turns, want 25", n)
	}
}
