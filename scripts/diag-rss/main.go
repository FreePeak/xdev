// Command diag-rss measures where a long xdev session's memory goes.
// It replays a real session file through the same code paths the TUI uses
// (session.Open -> Store.Entries -> session.BuildContext) and reports the
// live heap after each stage.
//
// It is a dev tool, not a shipped command:
//
//	go run ./scripts/diag-rss -session ~/.xdev/agent/sessions/<id>.jsonl
//
// The PRD (§3.5) names the session store as the largest item left in the
// memory budget: a session with no compaction boundary keeps its WHOLE
// decoded session resident (28 MB for a 19 MB session). This is the tool
// that measures it, so the next fix can be measured rather than guessed.
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/session"
)

func heapMB() float64 {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return float64(ms.HeapAlloc) / (1 << 20)
}

// blockBytes estimates the retained bytes of a block's text payload.
func blockBytes(b ai.Block) int {
	switch t := b.(type) {
	case ai.TextBlock:
		return len(t.Text)
	case ai.ThinkingBlock:
		return len(t.Thinking) + len(t.ThinkingSignature)
	case ai.ToolCallBlock:
		return len(t.ID) + len(t.Name) + len(t.Arguments) + len(t.PartialArgs) + len(t.Intent) + len(t.Signature)
	case ai.ImageBlock:
		return len(t.Source.Data) + len(t.Source.Type) + len(t.Source.MediaType)
	}
	return 0
}

func main() {
	path := flag.String("session", "", "path to a session .jsonl file")
	flag.Parse()
	if *path == "" {
		fmt.Fprintln(os.Stderr, "usage: diag-rss -session <file.jsonl>")
		os.Exit(2)
	}

	base := heapMB()
	fmt.Printf("baseline heap           %8.1f MB\n", base)

	st, err := session.Open(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	afterOpen := heapMB()
	fmt.Printf("after session.Open      %8.1f MB  (+%.1f)\n", afterOpen, afterOpen-base)

	entries := st.Entries()
	afterEntries := heapMB()
	fmt.Printf("after Entries()         %8.1f MB  (+%.1f)  n=%d\n",
		afterEntries, afterEntries-afterOpen, len(entries))

	leaf := ""
	if len(entries) > 0 {
		leaf = entries[len(entries)-1].Envelope().ID
	}
	ctx, err := session.BuildContext(entries, leaf, session.SystemPrompt{Text: "system"})
	if err != nil {
		fmt.Fprintln(os.Stderr, "buildContext:", err)
		os.Exit(1)
	}
	afterCtx := heapMB()
	fmt.Printf("after BuildContext      %8.1f MB  (+%.1f)  messages=%d\n",
		afterCtx, afterCtx-afterEntries, len(ctx.Messages))

	msgBytes := 0
	for _, m := range ctx.Messages {
		for _, b := range m.Content {
			msgBytes += blockBytes(b)
		}
	}
	fmt.Printf("context block bytes     %8.1f MB (sum of retained text/payload)\n", float64(msgBytes)/(1<<20))

	runtime.KeepAlive(ctx)
	runtime.KeepAlive(entries)
	runtime.KeepAlive(st)
}
