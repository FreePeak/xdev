package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// The badge upload path against a Bot API stub: a switched turn must change
// the bot's profile photo (that is the picture the user asked for) and must
// caption the answer with the leg that served it.
func TestApplyModelBadgeUploadsAndDedupes(t *testing.T) {
	var mu sync.Mutex
	uploads := 0
	var lastContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/setMyProfilePhoto") {
			writeOK(w, true)
			return
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		uploads++
		lastContentType = r.Header.Get("Content-Type")
		mu.Unlock()
		writeOK(w, true)
	}))
	t.Cleanup(srv.Close)

	d := &Daemon{tg: &Telegram{Token: "111111111:TESTtestTESTtestTESTtestTESTtestTESTtest",
		BaseURL: srv.URL, HTTPClient: srv.Client()}}
	ctx := context.Background()
	d.applyModelBadge(ctx, "opencode-free/big-pickle")
	d.applyModelBadge(ctx, "opencode-free/big-pickle") // same leg: no second upload
	d.applyModelBadge(ctx, "p2/m2")

	mu.Lock()
	n, ct := uploads, lastContentType
	mu.Unlock()
	if n != 2 {
		t.Fatalf("uploads = %d, want 2 (one per distinct leg, no re-upload of the same one)", n)
	}
	if !strings.HasPrefix(ct, "multipart/form-data") {
		t.Fatalf("content type = %q, want multipart/form-data", ct)
	}
}

// A bot the operator never granted photo rights must still answer: the badge
// is best-effort, so a refused upload is logged and the turn continues.
func TestApplyModelBadgeFailureIsNotFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":403,"description":"Forbidden: bot is not an administrator"}`))
	}))
	t.Cleanup(srv.Close)
	var logged []string
	d := &Daemon{
		tg:   &Telegram{Token: "111111111:TESTtestTESTtestTESTtestTESTtestTESTtest", BaseURL: srv.URL, HTTPClient: srv.Client()},
		logf: func(f string, a ...any) { logged = append(logged, f) },
	}
	d.applyModelBadge(context.Background(), "p1/m1") // must not panic
	if len(logged) == 0 {
		t.Fatal("a refused upload must be logged")
	}
	// A refusal must not be remembered as success: the next turn retries, so a
	// permission fixed mid-session takes effect without a restart.
	if d.badgeLeg != "" {
		t.Fatalf("badgeLeg = %q after a refusal, want empty", d.badgeLeg)
	}
}
