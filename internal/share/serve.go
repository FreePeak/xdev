package share

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// Server serves one sealed snapshot over loopback: "/<id>" is the view-only
// viewer page, "/<id>/blob" is the ciphertext. The key is carried by the
// link's #fragment, which a browser never sends to the server — the server
// holds the key only to print the link, and no HTTP response ever contains
// it. A different path is a 404, so one instance never serves another's
// snapshot.
type Server struct {
	id   string
	key  []byte
	blob []byte
	ln   net.Listener
	srv  *http.Server
}

// NewServer builds a server for one sealed snapshot.
func NewServer(id string, blob, key []byte) *Server {
	return &Server{id: id, blob: blob, key: key}
}

// Start binds the loopback listener (port 0 picks a free one) and serves in
// the background until Close.
func (s *Server) Start(port int) error {
	if s.id == "" {
		return errors.New("share: serve: empty snapshot id")
	}
	if len(s.blob) == 0 {
		return errors.New("share: serve: empty snapshot blob")
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("share: listen: %w", err)
	}
	s.ln = ln
	mux := http.NewServeMux()
	mux.HandleFunc("/"+s.id, s.serveViewer)
	mux.HandleFunc("/"+s.id+"/blob", s.serveBlob)
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = s.srv.Serve(ln) }()
	return nil
}

// Link is the share URL: the viewer page plus the key in the fragment.
func (s *Server) Link() string {
	if s.ln == nil {
		return ""
	}
	return "http://" + s.ln.Addr().String() + "/" + s.id + "#" + EncodeKey(s.key)
}

// Addr is the bound loopback address ("" before Start).
func (s *Server) Addr() string {
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Close stops serving.
func (s *Server) Close() error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Close()
}

// Blob returns the ciphertext this server hands out (tests + transport).
func (s *Server) Blob() []byte { return s.blob }

func (s *Server) serveViewer(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/"+s.id {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	// The page needs inline CSS + one inline script; the decrypted payload
	// renders inside a sandboxed (script-free) iframe. frame-src lists the
	// schemes a reader may pick up for about:srcdoc/blob: frames.
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; frame-src 'self' about: blob:")
	_, _ = w.Write([]byte(viewerPage))
}

func (s *Server) serveBlob(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/"+s.id+"/blob" {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(s.blob)
}

// viewerPage is the whole share viewer: no external asset. The embedded
// script reads the key from the URL fragment, fetches the ciphertext,
// decrypts it with WebCrypto AES-GCM, gunzips it and hands the resulting
// HTML to a sandboxed iframe — read-only by construction, since the payload
// cannot run script and the viewer exposes no write path.
const viewerPage = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>xdev shared session</title>
<style>
:root { color-scheme: light dark; }
body { margin: 0; height: 100vh; display: flex; flex-direction: column; background: #14141a; color: #e7e7ea;
  font: 14px/1.5 ui-sans-serif, -apple-system, "Segoe UI", Roboto, sans-serif; }
#status { margin: auto; padding: 2rem; text-align: center; color: #9a9aa4; }
#status small { display: block; margin-top: .5rem; }
#view { flex: 1; width: 100%; border: 0; background: #fff; display: none; }
@media (prefers-color-scheme: light) { body { background: #f6f5f1; color: #1b1b1a; } #status { color: #6b6b66; } }
</style>
</head>
<body>
<div id="status">decrypting snapshot…<small>the key stays in this URL's #fragment; it is never sent to the server</small></div>
<iframe id="view" sandbox referrerpolicy="no-referrer" title="xdev session"></iframe>
<script>
(function () {
  "use strict";
  var status = document.getElementById("status");
  var frame = document.getElementById("view");

  function fail(message) {
    status.textContent = "cannot open snapshot: " + message;
  }

  function b64u(text) {
    text = text.replace(/-/g, "+").replace(/_/g, "/");
    while (text.length % 4) { text += "="; }
    var bin = atob(text);
    var out = new Uint8Array(bin.length);
    for (var i = 0; i < bin.length; i++) { out[i] = bin.charCodeAt(i); }
    return out;
  }

  (async function () {
    try {
      var keyText = location.hash.replace(/^#/, "");
      if (!keyText) { throw new Error("missing key in the URL #fragment"); }
      var key = await crypto.subtle.importKey("raw", b64u(keyText), { name: "AES-GCM" }, false, ["decrypt"]);
      var res = await fetch(location.pathname + "/blob", { cache: "no-store" });
      if (!res.ok) { throw new Error("blob fetch failed (" + res.status + ")"); }
      var sealed = new Uint8Array(await res.arrayBuffer());
      if (sealed.length <= 12) { throw new Error("snapshot is truncated"); }
      var plain = await crypto.subtle.decrypt({ name: "AES-GCM", iv: sealed.subarray(0, 12) }, key, sealed.subarray(12));
      var html = await new Response(new Response(plain).body.pipeThrough(new DecompressionStream("gzip"))).text();
      frame.srcdoc = html;
      frame.style.display = "block";
      status.remove();
    } catch (err) {
      fail(err && err.message ? err.message : String(err));
    }
  })();
})();
</script>
</body>
</html>
`
