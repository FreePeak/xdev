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
