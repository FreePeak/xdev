package collab

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
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
			// No such thing as "upgraded": writable = f.Writable &&
			// Link.Full(), so a view-only link stays view-only. The old row
			// pinned "write permission granted", which promised a capability
			// Prompt() then refused — the banner must name the mismatch.
			name:     "view-only link, host claims write granted",
			link:     view,
			frame:    Frame{Name: "hostbox", Writable: true},
			welcomed: true,
			want:     []string{"carries no write token", "rejoin with the full link"},
			deny:     []string{"write permission granted"},
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
			// An empty want/deny row MEANS "print nothing"; without this the
			// loop would iterate zero times and pass vacuously.
			if tc.want == nil && tc.deny == nil && lines != nil {
				t.Errorf("this frame must print nothing; got %q", joined)
			}
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

	// The guest dispatches welcomes on its own goroutine, so the banner is
	// shared state and must be guarded (the -race build caught this test
	// reading it unlocked from the test goroutine).
	var (
		mu      sync.Mutex
		greeted bool
		banner  []string
	)
	welcome := func(f Frame) {
		mu.Lock()
		defer mu.Unlock()
		lines, now := joinWelcomeLines(link, f, greeted)
		greeted = now
		banner = append(banner, lines...)
	}
	g, err := Join(context.Background(), GuestConfig{
		Link:       link,
		ReplicaDir: t.TempDir(),
		OnWelcome:  welcome,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	runErr := make(chan error, 1)
	go func() { runErr <- g.Run(context.Background()) }()

	deadline := time.After(5 * time.Second)
	got := false
	for !got {
		select {
		case <-deadline:
			t.Fatal("no welcome arrived")
		case <-time.After(20 * time.Millisecond):
		}
		mu.Lock()
		got = greeted && len(banner) >= 1
		mu.Unlock()
	}
	// Give any second welcome frame time to land before judging.
	time.Sleep(300 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
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

// The optimistic banner's counterpart: a FULL link whose hello is rejected
// (wrong write token) still greets "full control", and the truth arrives
// separately as an EVENT notice — which is why joinWelcomeLines must never
// re-answer the mode. Frames flow through the real host, so this pins the
// ordering (banner first, correction second) at the seam that actually
// carries it.
func TestRejectedHelloGreetsOptimisticallyThenGetsNoticed(t *testing.T) {
	_, linkRaw := testHost(t, Backend{Snapshot: func() []byte { return nil }}, nil)
	link, err := ParseLink(linkRaw)
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewRoom()
	if err != nil {
		t.Fatal(err)
	}
	link.Write = other.Write // right length, wrong token: Full() stays true
	if !link.Full() {
		t.Fatal("fixture must still look like a full link")
	}

	var (
		mu      sync.Mutex
		banner  []string
		notices []string
		greeted bool
	)
	g, err := Join(context.Background(), GuestConfig{
		Link:       link,
		ReplicaDir: t.TempDir(),
		OnWelcome: func(f Frame) {
			mu.Lock()
			defer mu.Unlock()
			lines, now := joinWelcomeLines(link, f, greeted)
			greeted = now
			banner = append(banner, lines...)
		},
		OnEvent: func(raw json.RawMessage) {
			if txt := NoticeText(raw); txt != "" {
				mu.Lock()
				notices = append(notices, txt)
				mu.Unlock()
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	go func() { _ = g.Run(context.Background()) }()

	deadline := time.Now().Add(6 * time.Second)
	for {
		mu.Lock()
		done := greeted && strings.Contains(strings.Join(notices, "\n"), "not prompt")
		mu.Unlock()
		if done || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if !greeted {
		t.Fatal("no welcome arrived")
	}
	text := strings.Join(banner, "\n")
	if !strings.Contains(text, "full control") {
		t.Fatalf("banner should stay optimistic from the link, got %q", text)
	}
	if !strings.Contains(strings.Join(notices, "\n"), "not prompt") {
		t.Fatalf("host's refusal notice never reached the guest (notices=%v)", notices)
	}
	// And the capability really is absent: prompting must fail locally.
	if g.Writable() {
		t.Fatal("guest became writable with a wrong token")
	}
	if err := g.Prompt("should be refused"); err == nil {
		t.Fatal("Prompt accepted input the host would refuse")
	}
}

// Security-relevant AND, asserted through the guest's own state: a hand-made
// Writable:true welcome must not upgrade a VIEW-ONLY link. The host sends that
// frame only after a token check it cannot pass, and a malicious relay could
// simply lie — so writability is the conjunction, not the frame.
func TestWelcomeCannotUpgradeAViewOnlyLink(t *testing.T) {
	_, linkRaw := testHost(t, Backend{Snapshot: func() []byte { return nil }}, nil)
	full, err := ParseLink(linkRaw)
	if err != nil {
		t.Fatal(err)
	}
	view := Link{Relay: full.Relay, RoomID: full.RoomID, Key: full.Key} // no token
	greeted := false
	var banner []string
	g, err := Join(context.Background(), GuestConfig{
		Link:       view,
		ReplicaDir: t.TempDir(),
		OnWelcome: func(f Frame) {
			lines, now := joinWelcomeLines(view, f, greeted)
			greeted = now
			banner = append(banner, lines...)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	go func() { _ = g.Run(context.Background()) }()
	time.Sleep(400 * time.Millisecond)

	if g.Writable() {
		t.Fatal("a view-only link became writable")
	}
	if err := g.Prompt("nope"); err == nil {
		t.Fatal("Prompt succeeded on a view-only link")
	}
	text := strings.Join(banner, "\n")
	if strings.Contains(text, "full control") {
		t.Fatalf("banner over-claimed for a tokenless link:\n%s", text)
	}
	if !strings.Contains(text, "view-only") {
		t.Fatalf("banner should state view-only:\n%s", text)
	}
}
