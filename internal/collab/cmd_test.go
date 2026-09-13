package collab

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The banner used to read the FIRST welcome frame, which reports
// Writable:false for every guest (the write token rides FrameHello, after the
// handshake — a URL fragment is never sent over HTTP). That made
// `xdev join <full-link>` print "view-only …" and then "write permission
// granted" two lines later. The link parsed locally is the authority.
func TestJoinWelcomeBannerDerivesModeFromTheLink(t *testing.T) {
	room, err := NewRoom()
	if err != nil {
		t.Fatalf("NewRoom: %v", err)
	}
	full, view := room, room.ViewOnly()
	if !full.Full() || view.Full() {
		t.Fatal("fixture links are backwards")
	}

	// Handshake frame: what the host sends before any hello is proved.
	handshake := Frame{Name: "hostbox", Writable: false}

	cases := []struct {
		name     string
		link     Link
		frame    Frame
		welcomed bool // false when this frame IS the greeting
		want     []string
		deny     []string
	}{
		{
			name:  "full link, first welcome",
			link:  full,
			frame: handshake,
			want:  []string{"full control (you can prompt and interrupt)"},
			// The contradiction this fix exists to remove.
			deny: []string{"view-only"},
		},
		{
			name:  "view-only link, first welcome",
			link:  view,
			frame: handshake,
			want: []string{
				"view-only (ask the host for the full link to prompt)",
				"view-only link: typed lines are not sent",
			},
		},
		{
			// Optimistic banner: a full link whose hello was rejected still
			// greeted as full control, so the second welcome must NOT quietly
			// deny it — the host's own view-only notice (an event frame,
			// asserted live) is what tells the guest the truth. Repeating a
			// mode line here would give two answers to the same question.
			name:     "full link, second welcome without write access",
			link:     full,
			frame:    Frame{Name: "hostbox", Writable: false},
			welcomed: true,
			want:     nil,
		},
		{
			name:     "view-only link upgraded by the host",
			link:     view,
			frame:    Frame{Name: "hostbox", Writable: true},
			welcomed: true,
			want:     []string{"write permission granted"},
		},
		{
			name:     "full link confirmed by the second welcome",
			link:     full,
			frame:    Frame{Name: "hostbox", Writable: true},
			welcomed: true,
			want:     nil, // nothing new: the banner already said it
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lines, now := joinWelcomeLines(tc.link, tc.frame, tc.welcomed)
			if !now {
				t.Fatal("a welcome must always leave the greeting consumed")
			}
			joined := strings.Join(lines, "\n")
			for _, w := range tc.want {
				if !strings.Contains(joined, w) {
					t.Errorf("banner missing %q; got %q", w, joined)
				}
			}
			for _, d := range tc.deny {
				if strings.Contains(joined, d) {
					t.Errorf("banner must not say %q; got %q", d, joined)
				}
			}
			// Room and host identity ride the greeting, never a later frame.
			if !tc.welcomed && !strings.Contains(joined, "hostbox") {
				t.Errorf("greeting lost the host name: %q", joined)
			}
		})
	}
}

// Against a real host: a full-control guest's output must never contain
// "view-only", and the double welcome must not produce a self-contradiction.
// This exercises the branch the table cannot — the second frame arriving from
// an actual server after an actual token check.
func TestFullControlGuestNeverHearsViewOnly(t *testing.T) {
	_, linkRaw := testHost(t, Backend{Snapshot: func() []byte { return nil }}, nil)
	link, err := ParseLink(linkRaw)
	if err != nil {
		t.Fatal(err)
	}
	if !link.Full() {
		t.Fatal("testHost must hand out a full link")
	}

	greeted := false
	var banner []string
	g, err := Join(context.Background(), GuestConfig{
		Link:       link,
		ReplicaDir: t.TempDir(),
		OnWelcome: func(f Frame) {
			lines, now := joinWelcomeLines(link, f, greeted)
			greeted = now
			banner = append(banner, lines...)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	runErr := make(chan error, 1)
	go func() { runErr <- g.Run(context.Background()) }()

	deadline := time.After(5 * time.Second)
	for !greeted || len(banner) < 1 {
		select {
		case <-deadline:
			t.Fatalf("no welcome arrived (banner=%v)", banner)
		case <-time.After(20 * time.Millisecond):
		}
	}
	// Give any second welcome frame time to land before judging.
	time.Sleep(300 * time.Millisecond)

	text := strings.Join(banner, "\n")
	if strings.Contains(text, "view-only") {
		t.Fatalf("full-control guest heard view-only:\n%s", text)
	}
	if !strings.Contains(text, "full control") {
		t.Fatalf("banner must state the capability the link grants:\n%s", text)
	}
	if n := strings.Count(text, "·"); n != 1 {
		t.Fatalf("banner should be one line, got %d:\n%s", n, text)
	}
	g.Close()
	select {
	case <-runErr:
	case <-time.After(3 * time.Second):
	}
}
