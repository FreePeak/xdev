package stats

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDashboardServesTotalsPageAndJSON(t *testing.T) {
	opts := Options{DataDir: fiveSessions(t), NoRollup: true}
	srv := httptest.NewServer(Handler(opts, time.Hour))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html", ct)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	page := string(body)
	for _, want := range []string{
		"xdev stats",
		"<td>turns</td><td>15</td>",
		"<td>sessions</td><td>5</td>",
		"1.7k",          // total tokens 1725
		"<td>bash</td>", // tool table
		"2026-09-12",    // per-day row
		"EventSource('/events')",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page missing %q", want)
		}
	}

	res, err = http.Get(srv.URL + "/api/report")
	if err != nil {
		t.Fatalf("GET /api/report: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("/api/report = %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	var rep Report
	if err := json.NewDecoder(res.Body).Decode(&rep); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if rep.Totals.Sessions != 5 || rep.Totals.Turns != 15 || rep.Totals.ToolCalls != 15 {
		t.Errorf("totals = %+v", rep.Totals)
	}

	if res, err = http.Get(srv.URL + "/nope"); err != nil {
		t.Fatalf("GET /nope: %v", err)
	} else {
		res.Body.Close()
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("GET /nope = %d, want 404", res.StatusCode)
		}
	}
}

func TestDashboardEventsStreamsRenderedFragment(t *testing.T) {
	opts := Options{DataDir: fiveSessions(t), NoRollup: true}
	srv := httptest.NewServer(Handler(opts, 10*time.Millisecond))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/events", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}

	br := bufio.NewReader(res.Body)
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read SSE: %v", err)
	}
	if !strings.HasPrefix(line, "data: ") {
		t.Fatalf("first SSE line = %q, want a data: frame", line)
	}
	var frag string
	if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data: "))), &frag); err != nil {
		t.Fatalf("SSE payload is not a JSON string: %v", err)
	}
	if !strings.Contains(frag, "<td>turns</td><td>15</td>") {
		t.Errorf("fragment missing totals: %q", frag)
	}
}

func TestCheckLoopback(t *testing.T) {
	ok := []string{"127.0.0.1:3847", "localhost:3847", "[::1]:3847", "127.0.0.1:0"}
	for _, addr := range ok {
		if err := checkLoopback(addr); err != nil {
			t.Errorf("checkLoopback(%q) = %v, want nil", addr, err)
		}
	}
	bad := []string{"0.0.0.0:3847", ":3847", "192.168.1.10:3847", "example.com:3847", "not-an-address"}
	for _, addr := range bad {
		if err := checkLoopback(addr); err == nil {
			t.Errorf("checkLoopback(%q) = nil, want an error (dashboard has no auth)", addr)
		}
	}
}

func TestServeRejectsNonLoopbackBind(t *testing.T) {
	err := Serve(context.Background(), "0.0.0.0:3847", Options{DataDir: t.TempDir()}, time.Second)
	if err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("Serve(0.0.0.0) = %v, want a loopback error", err)
	}
}

func TestServeStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, "127.0.0.1:0", Options{DataDir: t.TempDir(), NoRollup: true}, time.Hour) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v, want nil after cancel", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return after context cancel")
	}
}
