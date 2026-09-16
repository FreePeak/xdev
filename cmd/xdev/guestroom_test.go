package main

import (
	"os"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/collab"
	"github.com/FreePeak/xdev/internal/tui"
)

// F5 re-runs the last prompt locally unless this session mirrors a room as a
// guest. Both send paths used to ask the WIRING instead of the ROOM
// (`tui.Collab != nil && tui.Collab.Forward != nil`), and every TUI installs
// that seam at startup — so F5 answered "joined as a guest — the host runs the
// turn" in plain local sessions, and a draft with a pasted image bounced with
// "a guest room forwards text only".

func TestGuestStateFollowsTheRoomNotTheWiring(t *testing.T) {
	old := tui.Collab
	t.Cleanup(func() {
		tui.Collab = old
		collabMu.Lock()
		collabGuest = nil
		collabMu.Unlock()
	})

	// The seam wired, no room joined: a plain local session. This is the state
	// every TUI starts in and the state the old guard misread as "guest".
	tui.Collab = &tui.CollabOps{Forward: func(string) bool { return false }}
	if collabGuestJoined() {
		t.Fatal("a local session with the collab seam installed read as a guest")
	}

	// Joined a room: the host owns the turn, so F5 must not start one here.
	collabMu.Lock()
	collabGuest = &collab.Guest{}
	collabMu.Unlock()
	if !collabGuestJoined() {
		t.Fatal("a joined guest did not read as a guest")
	}
}

// TestSendPathsGateOnTheRoom pins the fix at both call sites. runTUI needs a
// tty, so a test cannot drive F5 through it; this reads the source instead and
// fails if a future edit re-gates a send path on the presence of the seam.
func TestSendPathsGateOnTheRoom(t *testing.T) {
	src, err := os.ReadFile("tui.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	if strings.Contains(s, "if tui.Collab != nil") {
		t.Fatal("a send path gates on the wiring again")
	}
	if n := strings.Count(s, "if collabGuestJoined() {"); n != 2 {
		t.Fatalf("want both send guards on collabGuestJoined, found %d", n)
	}
}
