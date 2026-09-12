package stats

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"strings"
	"time"
)

// Handler is the read-only dashboard: "/" (server-rendered totals, live via
// SSE), "/api/report" (the JSON report) and "/events" (SSE, one event per
// refresh). No route mutates anything, so a stray page load can never touch
// the session store.
func Handler(opts Options, refresh time.Duration) http.Handler {
	if refresh <= 0 {
		refresh = DefaultRefresh
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		rep, err := Scan(opts)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		page, err := renderPage(rep)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(page))
	})
	mux.HandleFunc("/api/report", func(w http.ResponseWriter, r *http.Request) {
		rep, err := Scan(opts)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.Encode(rep)
	})
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		serveEvents(w, r, opts, refresh)
	})
	return mux
}

// serveEvents pushes a freshly rendered fragment per tick. Each client owns
// its own ticker and scan; the scan is bounded (Options caps) and cheap
// after the first pass (rollup cache), so N tabs cost N bounded scans.
//
// ponytail: N clients = N scans per tick. Upgrade path: one shared ticker
// broadcasting a single report if the dashboard is ever left open in many
// tabs against a store too large for the rollup to cover.
func serveEvents(w http.ResponseWriter, r *http.Request, opts Options, refresh time.Duration) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	send := func() bool {
		rep, err := Scan(opts)
		if err != nil {
			b, _ := json.Marshal(err.Error())
			fmt.Fprintf(w, "event: error\ndata: %s\n\n", b)
			fl.Flush()
			return false
		}
		frag, err := renderFragment(rep)
		if err != nil {
			return false
		}
		// JSON-encode the fragment so a multi-line payload stays one SSE
		// data line (the client JSON.parses it back before innerHTML).
		// HTML escaping is off: the payload is a string, not markup to
		// re-escape (<table> doubles in size as \u003ctable\u003e).
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(frag); err != nil {
			return false
		}
		fmt.Fprintf(w, "data: %s\n", bytes.TrimRight(buf.Bytes(), "\n"))
		fl.Flush()
		return true
	}
	if !send() {
		return
	}
	t := time.NewTicker(refresh)
	defer t.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-t.C:
			if !send() {
				return
			}
		}
	}
}

// Serve runs the dashboard on addr until ctx is cancelled. addr must be a
// loopback address: the dashboard renders session titles, working
// directories and model usage with no authentication.
func Serve(ctx context.Context, addr string, opts Options, refresh time.Duration) error {
	if err := checkLoopback(addr); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("stats: listen %s: %w", addr, err)
	}
	srv := &http.Server{Handler: Handler(opts, refresh), ReadHeaderTimeout: 10 * time.Second}
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(shutCtx)
	}()
	err = srv.Serve(ln)
	<-done
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// checkLoopback rejects a non-loopback bind: with no auth, an accidental
// 0.0.0.0 would publish the user's session metadata to the network.
func checkLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("stats: bad address %q: %w", addr, err)
	}
	if host == "" {
		return fmt.Errorf("stats: address %q has no host (use 127.0.0.1); the dashboard has no authentication", addr)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("stats: %q is not a loopback address; the dashboard has no authentication", host)
	}
	return nil
}

// --- rendering ---

// tmplFuncs must be registered before Parse: an unregistered {{tok}} makes
// template.Must panic at init.
var tmplFuncs = template.FuncMap{
	"tok":   HumanTokens,
	"money": HumanMoney,
	"stamp": func(t time.Time) string {
		if t.IsZero() {
			return "—"
		}
		return t.UTC().Format("2006-01-02")
	},
}

// pageTemplate wraps the fragment; fragmentTemplate is the live payload the
// SSE stream swaps into #live (one render path for both).
var pageTemplate = template.Must(template.New("page").Funcs(tmplFuncs).Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>xdev stats</title>
<style>
:root{color-scheme:light dark}
body{font:13px/1.5 ui-monospace,SFMono-Regular,Menlo,monospace;margin:0;padding:1.75rem;
     background:#fbfbfd;color:#1b1d22}
@media (prefers-color-scheme:dark){body{background:#14161a;color:#e8e8ea}}
h1{font-size:1.05rem;margin:0 0 .2rem}
.meta{opacity:.6;margin-bottom:1.4rem}
h2{font-size:.72rem;text-transform:uppercase;letter-spacing:.08em;opacity:.6;margin:1.6rem 0 .4rem}
table{border-collapse:collapse;min-width:26rem}
th,td{text-align:right;padding:.15rem .8rem .15rem 0}
th:first-child,td:first-child{text-align:left}
thead th{border-bottom:1px solid currentColor;font-weight:600;opacity:.65}
.cols{display:flex;gap:2.5rem;flex-wrap:wrap;align-items:flex-start}
.quiet{opacity:.55}
</style></head>
<body>
<h1>xdev stats</h1>
<div class="meta">{{.Meta}} · <span id="dot">live</span></div>
<div id="live">{{.Fragment}}</div>
<script>
const live=document.getElementById('live');
const dot=document.getElementById('dot');
new EventSource('/events').onmessage=function(e){
  live.innerHTML=JSON.parse(e.data);
  dot.textContent='live '+new Date().toLocaleTimeString();
};
</script>
</body></html>
`))

var fragmentTemplate = template.Must(template.New("frag").Funcs(tmplFuncs).Parse(`<table>
<thead><tr><th>metric</th><th>value</th></tr></thead>
<tbody>
<tr><td>sessions</td><td>{{.Totals.Sessions}}</td></tr>
<tr><td>subagent sessions</td><td>{{.Totals.Subagents}}</td></tr>
<tr><td>turns</td><td>{{.Totals.Turns}}</td></tr>
<tr><td>user messages</td><td>{{.Totals.UserMessages}}</td></tr>
<tr><td>tool calls</td><td>{{.Totals.ToolCalls}} ({{.Totals.ToolErrors}} errors)</td></tr>
<tr><td>tokens in / out</td><td>{{tok .Totals.Input}} / {{tok .Totals.Output}}</td></tr>
<tr><td>cache read / write</td><td>{{tok .Totals.CacheRead}} / {{tok .Totals.CacheWrite}}</td></tr>
<tr><td>total tokens</td><td>{{tok .Totals.TotalTokens}}</td></tr>
<tr><td>cost (reported)</td><td>{{money .Totals.CostUSD}} over {{.Totals.PricedTurns}} priced turns</td></tr>
<tr><td>window</td><td>{{stamp .Totals.FirstSession}} &rarr; {{stamp .Totals.LastSession}}</td></tr>
<tr><td>per session</td><td>turns p50 {{.Distribution.TurnsP50}} · p90 {{.Distribution.TurnsP90}} · max {{.Distribution.TurnsMax}}</td></tr>
<tr><td>per session</td><td>tokens p50 {{tok .Distribution.TokensP50}} · p90 {{tok .Distribution.TokensP90}} · max {{tok .Distribution.TokensMax}}</td></tr>
<tr><td>files</td><td>{{.FilesScanned}} scanned · {{.CacheHits}} cached · {{.FilesOmitted}} over cap{{if .Truncated}} (truncated){{end}}</td></tr>
</tbody></table>

<div class="cols">
<div>
<h2>by model</h2>
{{if .Models}}<table>
<thead><tr><th>model</th><th>turns</th><th>tokens</th><th>cost</th></tr></thead>
<tbody>{{range .Models}}<tr><td>{{.Model}}</td><td>{{.Turns}}</td><td>{{tok .TotalTokens}}</td><td>{{money .CostUSD}}</td></tr>
{{end}}</tbody></table>
{{else}}<div class="quiet">no turns recorded</div>{{end}}
</div>
<div>
<h2>top tools</h2>
{{if .Tools}}<table>
<thead><tr><th>tool</th><th>calls</th><th>errors</th></tr></thead>
<tbody>{{range .Tools}}<tr><td>{{.Name}}</td><td>{{.Calls}}</td><td>{{.Errors}}</td></tr>
{{end}}</tbody></table>
{{else}}<div class="quiet">no tool calls recorded</div>{{end}}
</div>
</div>

<h2>per day (UTC)</h2>
{{if .Days}}<table>
<thead><tr><th>day</th><th>sessions</th><th>turns</th><th>tokens</th><th>cost</th></tr></thead>
<tbody>{{range .Days}}<tr><td>{{.Day}}</td><td>{{.Sessions}}</td><td>{{.Turns}}</td><td>{{tok .Tokens}}</td><td>{{money .CostUSD}}</td></tr>
{{end}}</tbody></table>
{{else}}<div class="quiet">no activity recorded</div>{{end}}
`))

func renderFragment(rep *Report) (string, error) {
	var b strings.Builder
	if err := fragmentTemplate.Execute(&b, rep); err != nil {
		return "", err
	}
	return b.String(), nil
}

func renderPage(rep *Report) (string, error) {
	frag, err := renderFragment(rep)
	if err != nil {
		return "", err
	}
	meta := fmt.Sprintf("%s · generated %s · scanned %d file(s)",
		rep.DataDir, rep.GeneratedAt.Format("2006-01-02 15:04:05Z"), rep.FilesScanned)
	var b strings.Builder
	if err := pageTemplate.Execute(&b, map[string]any{
		"Meta":     meta,
		"Fragment": template.HTML(frag),
	}); err != nil {
		return "", err
	}
	return b.String(), nil
}

// --- shared formatting (CLI table + dashboard) ---

// HumanTokens renders a token count compactly (1234 → "1.2k").
func HumanTokens(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.1fB", float64(n)/1e9)
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 10_000:
		return fmt.Sprintf("%.0fk", float64(n)/1e3)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// HumanMoney renders USD with the precision usage tracking needs ($0.0000
// for sub-cent sums).
func HumanMoney(v float64) string { return fmt.Sprintf("$%.4f", v) }
