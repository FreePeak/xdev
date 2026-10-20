package gateway

import "sync"

// The bridge answers a chat while another chat is still working, and one
// chat's own messages stay in the order they arrived.
//
// A per-chat FIFO is the whole mechanism. The poll loop pushes a turn and
// goes straight back to polling; one runner goroutine per chat drains that
// chat's queue in order. Two properties fall out of it:
//
//   - Order is preserved. Two messages in one poll batch must not race for
//     the session: the second one has to be the second turn, or the model
//     reads the conversation backwards.
//   - A wedged turn is one chat's problem. The runner is per chat, so a
//     worker that never returns cannot freeze the bridge for the others —
//     and /stop, which the poll loop reads, stays reachable.
//
// The queue is unbounded on purpose. A phone user may send a burst of
// messages, and every one of them deserves an answer: the work they cost is
// the work the user asked for.
//
// ponytail: one map, one mutex, one slice per chat. A condition variable or
// a linked list would buy nothing here.
type turnQueue struct {
	mu    sync.Mutex
	chats map[int64]*chatTurns
	run   func(chat int64, text string)
}

// chatTurns is one chat's pending turns.
type chatTurns struct {
	mu    sync.Mutex
	items []string
	// wake carries the "there is work" nudge to the runner. It holds one
	// and never blocks the pusher: a dropped wake would strand a queued
	// turn, so the pusher always tries to fill it, and the runner always
	// re-checks the items before it waits.
	wake chan struct{}
}

func newTurnQueue(run func(chat int64, text string)) *turnQueue {
	return &turnQueue{chats: map[int64]*chatTurns{}, run: run}
}

// push queues a turn for the chat and makes sure a runner exists for it.
func (q *turnQueue) push(chat int64, text string) {
	q.mu.Lock()
	c, ok := q.chats[chat]
	if !ok {
		c = &chatTurns{wake: make(chan struct{}, 1)}
		q.chats[chat] = c
		go q.drain(chat, c)
	}
	q.mu.Unlock()

	c.mu.Lock()
	c.items = append(c.items, text)
	c.mu.Unlock()

	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// drain runs one chat's turns, in order, for as long as the daemon runs. The
// runner lives as long as the daemon: one goroutine per chat that ever sent
// a message is a price worth paying for a bridge that never wedges.
func (q *turnQueue) drain(chat int64, c *chatTurns) {
	for range c.wake {
		for {
			c.mu.Lock()
			if len(c.items) == 0 {
				c.mu.Unlock()
				break
			}
			text := c.items[0]
			c.items = c.items[1:]
			c.mu.Unlock()
			q.run(chat, text)
		}
	}
}
