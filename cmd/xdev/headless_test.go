package main

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/FreePeak/xdev/internal/serve"
)

// TestParseSSEFrames checks the client's parser: the frames a server writes
// (event/data pairs separated by a blank line) come back one by one, with the
// heartbeat comment lines dropped.
func TestParseSSEFrames(t *testing.T) {
	body := "event: server.connected\ndata: {}\n\n" +
		": heartbeat\n\n" +
		"event: event\ndata: {\"id\":\"p1\"}\n\n"
	ch := make(chan sseFrame, 8)
	go readSSE(strings.NewReader(body), ch)
	var got []string
	for f := range ch {
		got = append(got, f.event+"|"+f.data)
	}
	if len(got) != 2 {
		t.Fatalf("frames = %v, want 2 (the comment line is not a frame)", got)
	}
	if got[0] != "server.connected|{}" {
		t.Errorf("frame 0 = %q, want server.connected|{}", got[0])
	}
	if got[1] != `event|{"id":"p1"}` {
		t.Errorf("frame 1 = %q, want event|{...}", got[1])
	}
}

// TestAttachWithoutAServerSaysSo pins the client's own error path: with no
// registration file the answer is the actionable one, not a connection failure.
func TestAttachWithoutAServerSaysSo(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dataDir)
	out := captureStdout(t, func() {
		if code := runHeadlessClient([]string{"hello"}); code != 1 {
			t.Errorf("exit = %d, want 1 (nothing to attach to)", code)
		}
	})
	if out != "" {
		t.Errorf("stdout = %q, want empty (the complaint goes to stderr)", out)
	}
}

// TestAttachPrintsTheRegistration exercises the --print path against a real
// server: the client discovers it by file and prints what it would connect to.
func TestAttachPrintsTheRegistration(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("XDEV_AGENT_DIR", dataDir)
	srv, err := serve.NewHeadless(serve.HeadlessOptions{
		Options: serve.Options{Listen: "127.0.0.1:0", Token: "tok", Version: "v-test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Bind through the public API before the client runs: the registration
	// only exists once the listener exists, and dispatching Run in a goroutine
	// with no synchronisation makes the client's read a race.
	addr, err := serve.BindForTest(srv)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(addr)
	waitForRegistration(t, serve.HeadlessRegistrationPath(dataDir))

	out := captureStdout(t, func() {
		if code := runHeadlessClient([]string{"--print"}); code != 0 {
			t.Errorf("exit = %d, want 0", code)
		}
	})
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--print output is not JSON: %v (%q)", err, out)
	}
	if got["version"] != "v-test" {
		t.Errorf("version = %v, want v-test", got["version"])
	}
	url, _ := got["url"].(string)
	if !strings.HasPrefix(url, "http://127.0.0.1:") {
		t.Errorf("url = %q, want the loopback the server bound", url)
	}
	// The token must NOT be printed: it is a credential, and `--print` exists
	// to be pasted into a chat or a ticket.
	if strings.Contains(out, "tok") {
		t.Errorf("--print leaked the token: %q", out)
	}
}

// waitForRegistration fails the test if path does not appear quickly.
func waitForRegistration(t *testing.T, path string) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if _, err := os.Stat(path); err == nil {
			return
		}
	}
	t.Fatalf("%s never appeared", path)
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what
// it wrote.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	os.Stdout = old
	_ = w.Close()
	return <-done
}
