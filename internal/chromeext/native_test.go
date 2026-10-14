package chromeext

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func frame(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(raw)))
	return append(hdr[:], raw...)
}

type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func TestNativeRoundTripListAndCDP(t *testing.T) {
	hostIn, extOut := io.Pipe()
	var hostOut bytes.Buffer
	var outMu sync.Mutex
	b := NewBridge(&lockedWriter{w: &hostOut, mu: &outMu})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv, err := NewServer(b, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	go func() { _ = srv.Serve(ctx) }()
	go func() { _ = b.ReadLoop(hostIn) }()

	go func() {
		for {
			outMu.Lock()
			raw := append([]byte(nil), hostOut.Bytes()...)
			hostOut.Reset()
			outMu.Unlock()
			for len(raw) >= 4 {
				n := int(binary.LittleEndian.Uint32(raw[:4]))
				if 4+n > len(raw) {
					outMu.Lock()
					hostOut.Write(raw)
					outMu.Unlock()
					break
				}
				msg := raw[4 : 4+n]
				raw = raw[4+n:]
				var env map[string]any
				if err := json.Unmarshal(msg, &env); err != nil {
					continue
				}
				typ, _ := env["type"].(string)
				id, _ := env["id"].(float64)
				switch typ {
				case "list_targets":
					_, _ = extOut.Write(frame(t, map[string]any{
						"type": "list_targets_result",
						"id":   int64(id),
						"targets": []map[string]any{
							{"id": "42", "type": "page", "title": "Example", "url": "https://example.com/"},
						},
					}))
				case "cdp":
					_, _ = extOut.Write(frame(t, map[string]any{
						"type":   "cdp_result",
						"id":     int64(id),
						"result": map[string]any{"ok": true},
					}))
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()

	if _, err := extOut.Write(frame(t, map[string]any{"type": "hello", "extensionId": ExtensionID})); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for !b.Connected() {
		if time.Now().After(deadline) {
			t.Fatal("not connected")
		}
		time.Sleep(5 * time.Millisecond)
	}

	addr := "http://" + srv.Addr()
	resp, err := http.Get(addr + "/json/list")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("list status %d: %s", resp.StatusCode, body)
	}
	var targets []Target
	if err := json.NewDecoder(resp.Body).Decode(&targets); err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].ID != "42" {
		t.Fatalf("targets = %+v", targets)
	}
	if !strings.Contains(targets[0].WebSocketDebuggerURL, "/devtools/page/42") {
		t.Fatalf("ws url = %q", targets[0].WebSocketDebuggerURL)
	}

	raw, err := b.CDP(context.Background(), "42", "Runtime.evaluate", json.RawMessage(`{"expression":"1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("ok")) {
		t.Fatalf("cdp result %s", raw)
	}
}

func TestInstallAndProbe(t *testing.T) {
	dir := t.TempDir()
	ext := t.TempDir()
	if err := os.WriteFile(filepath.Join(ext, "manifest.json"), []byte(`{"manifest_version":3,"name":"t"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "xdev-bin")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	nm := filepath.Join(dir, "NativeMessagingHosts", hostManifestName)
	registerNativeHostFn = func(hostMan string) ([]string, string, error) {
		if err := os.MkdirAll(filepath.Dir(nm), 0o755); err != nil {
			return nil, "", err
		}
		raw, err := os.ReadFile(hostMan)
		if err != nil {
			return nil, "", err
		}
		if err := os.WriteFile(nm, raw, 0o644); err != nil {
			return nil, "", err
		}
		return []string{"test-browser"}, nm, nil
	}
	unregisterNativeHostFn = func() error { return os.Remove(nm) }
	nativeManifestPathFn = func() string { return nm }
	t.Cleanup(func() {
		registerNativeHostFn = registerNativeHost
		unregisterNativeHostFn = unregisterNativeHost
		nativeManifestPathFn = nativeManifestPath
	})
	res, err := Install(InstallOptions{DataDir: dir, XdevBin: bin, ExtensionSource: ext})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExtensionID != ExtensionID {
		t.Fatalf("id %s", res.ExtensionID)
	}
	st := Probe(dir)
	if !st.ExtensionOK || !st.HostWrapperOK || !st.NativeOK {
		t.Fatalf("probe incomplete: %+v", st)
	}
	if err := Uninstall(dir); err != nil {
		t.Fatal(err)
	}
}

func TestSendRejectsHugeFrame(t *testing.T) {
	var buf bytes.Buffer
	b := NewBridge(&buf)
	// Marshal of a string adds quotes; size the payload so the JSON exceeds the cap.
	huge := strings.Repeat("x", maxNativeFrame)
	if err := b.Send(huge); err == nil {
		t.Fatal("expected too-large error")
	}
}
