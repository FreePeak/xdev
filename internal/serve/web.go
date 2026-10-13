// The web client: one embedded page over the same routes every other headless
// client uses. There is no build step and no asset pipeline — the page is a
// single HTML file with inline CSS and JavaScript, embedded in the binary with
// go:embed, so `xdev serve headless` stays one static CGO-free binary and
// `GET /` works on a machine with nothing installed.
//
// OpenCode ships a bundled SPA (Solid + rolldown) at its `opencode serve`
// root; the SHAPE is adopted here — the daemon serves the UI, so the user gets
// a link instead of an API to write a client against — while the build system
// is not: a node toolchain in a Go repo is a second thing to install, to
// break, and to keep in the CGO-free binary's size budget.
//
// What the page does, and the contract it leans on:
//
//   - reads the token from the URL FRAGMENT, exactly as `xdev share` carries
//     its key: a fragment is never sent to the server, so the token does not
//     land in a log or a Referer header, and the page strips it from the
//     address bar after reading it;
//   - GET /history for the durable transcript (the export shape, so the page
//     and `xdev export` cannot drift), then GET /event for liveness;
//   - POST /prompt, POST /steer, POST /abort, GET /state for the rest.
//
// The stream is volatile by contract, so the page treats a dropped or
// overflowed stream as "re-read the transcript and reconnect" rather than
// hiding it: truth is the store, the stream is only what is happening now.
package serve

import (
	_ "embed"
	"net/http"
)

//go:embed web/index.html
var webIndexHTML []byte

// handleIndex serves the web client. It carries no session data — every fact
// the page shows is fetched from a token-gated route — so serving it before
// the caller holds a token leaks nothing, and it has to be reachable that way:
// a browser navigating to the URL cannot attach an Authorization header, and
// the token is in the fragment precisely so it never needs to.
func (h *Headless) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// No framing: the page drives an agent, and a page that can be framed is a
	// clickjacking surface over /abort and /prompt.
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(webIndexHTML)
}
