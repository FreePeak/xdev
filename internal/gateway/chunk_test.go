package gateway

import (
	"context"
	"strings"
	"testing"
)

// A 5000-char reply to a 120-char replyChunk must arrive as ~42 sendMessage
// calls, each under the 4096 hard cap, reassembling exactly.
func TestDaemonChunksLongReply(t *testing.T) {
	b := newFakeBot(t)
	b.queue(textUpdate("anything", 7))
	d := newTestDaemon(t, b, []int64{7})
	d.chunk = 120 // force the small chunk size
	// 5000 'a' is 5000 UTF-16 units; the chunker must split it.
	reply := strings.Repeat("a", 5000)
	if err := d.reply(context.Background(), 7, reply); err != nil {
		t.Fatalf("send: %v", err)
	}
	msgs := b.sentTo(7)
	if len(msgs) < 30 {
		t.Fatalf("want ~42 chunks for 5000 units at a 120-unit cap, got %d", len(msgs))
	}
	for i, m := range msgs {
		if len(m) > d.chunk {
			t.Errorf("chunk %d = %d chars, want <= %d", i, len(m), d.chunk)
		}
		if len(m) > TelegramMessageLimit {
			t.Errorf("chunk %d exceeds the Telegram cap", i)
		}
	}
	if got := strings.Join(msgs, ""); got != reply {
		t.Errorf("chunks do not rebuild the reply")
	}
}

// TestReplyStopsAtFirstFailedChunk: a send that fails halfway must not leave
// the chat with a truncated answer and no explanation. The error is returned;
// the log line that follows it is the operator's record.
func TestReplyStopsAtFirstFailedChunk(t *testing.T) {
	b := newFakeBot(t)
	b.queue(textUpdate("anything", 7))
	d := newTestDaemon(t, b, []int64{7})
	d.chunk = 10 // 50 units → 5 chunks
	b.failAfter = 3
	err := d.reply(context.Background(), 7, strings.Repeat("a", 50))
	if err == nil {
		t.Fatal("a failed send must be reported, not swallowed")
	}
	if got := len(b.sentTo(7)); got != 3 {
		t.Errorf("sent %d chunks, want exactly the 3 that succeeded", got)
	}
}

// TestUnboundedOutputIsBounded: a worker that prints without bound must not
// grow an unbounded in-memory buffer, and the tail (the answer) must survive.
func TestUnboundedOutputIsBounded(t *testing.T) {
	var buf ringBuffer
	buf.limit = 1024
	// A preamble bigger than the cap, then the answer.
	_, _ = buf.Write([]byte(strings.Repeat("x", 10_000)))
	_, _ = buf.Write([]byte("the-answer"))
	out := buf.String()
	if !strings.HasSuffix(out, "the-answer") {
		t.Errorf("the answer was dropped: %q", out[len(out)-20:])
	}
	if len(out) > 1024+64 {
		t.Errorf("buffer grew to %d bytes, want <= the cap plus the marker", len(out))
	}
	if !strings.Contains(out, "dropped") {
		t.Error("a dropped-prefix marker must say the output was truncated")
	}
}
