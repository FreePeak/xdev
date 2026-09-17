package share

import "html/template"

// exportTmpl renders one Document. It is deliberately one self-contained
// page: inline CSS, no external asset, no JavaScript — reading the export
// needs nothing but a browser. Every interpolated value goes through
// html/template's contextual escaping, so markup inside a transcript entry
// is displayed as text and can never run.
//
// Layout: a metadata header (session id, model, cwd, timestamps, leaf),
// the system prompt, then every entry in file order.
var exportTmpl = template.Must(template.New("export").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="generator" content="xdev export">
<title>{{if .Title}}{{.Title}}{{else}}xdev session{{end}}</title>
<style>
:root {
  --bg: #fdfdfc; --fg: #1b1b1a; --dim: #6b6b66; --line: #e3e2dd;
  --card: #ffffff; --side: #f6f5f1; --accent: #7a5cff;
  --user: #2f6f4f; --assistant: #2b5d8a; --tool: #8a5a2b; --think: #6b6b66;
}
@media (prefers-color-scheme: dark) {
  :root {
    --bg: #14141a; --fg: #e7e7ea; --dim: #9a9aa4; --line: #2c2c36;
    --card: #1b1b22; --side: #20202a; --accent: #a892ff;
    --user: #7cc79c; --assistant: #8ab8e8; --tool: #e0b070; --think: #9a9aa4;
  }
}
* { box-sizing: border-box; }
body {
  margin: 0; padding: 2rem 1.25rem 4rem; background: var(--bg); color: var(--fg);
  font: 15px/1.55 ui-sans-serif, -apple-system, "Segoe UI", Roboto, sans-serif;
}
main, header.meta, section.system, footer { max-width: 60rem; margin: 0 auto; }
h1 { font-size: 1.35rem; margin: 0 0 .75rem; }
h2 { font-size: 1rem; margin: 0 0 .5rem; color: var(--dim); text-transform: uppercase; letter-spacing: .06em; }
dl { display: grid; grid-template-columns: auto 1fr; gap: .15rem 1rem; margin: 0; font-size: .85rem; }
dl > div { display: contents; }
dt { color: var(--dim); }
dd { margin: 0; overflow-wrap: anywhere; }
code, pre, time { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
header.meta { border-bottom: 1px solid var(--line); padding-bottom: 1rem; }
section.system { margin-top: 1.5rem; }
section.system, .part { background: var(--side); border: 1px solid var(--line); border-radius: 8px; }
section.system pre, .part pre { margin: 0; padding: .6rem .75rem; white-space: pre-wrap; overflow-wrap: anywhere; font-size: .82rem; }
.entry { margin: 1rem 0; border: 1px solid var(--line); border-radius: 10px; background: var(--card); overflow: hidden; }
.entry > header { display: flex; flex-wrap: wrap; gap: .5rem; align-items: baseline; padding: .45rem .75rem; background: var(--side); border-bottom: 1px solid var(--line); font-size: .8rem; }
.entry .kind { font-weight: 600; letter-spacing: .02em; }
.entry time, .entry .note { color: var(--dim); }
.entry .note { font-family: ui-monospace, monospace; font-size: .75rem; }
.entry.user .kind { color: var(--user); }
.entry.assistant .kind { color: var(--assistant); }
.entry.tool_result .kind, .entry.custom .kind { color: var(--tool); }
.entry.model_change, .entry.compaction, .entry.branch_summary,
.entry.reset_boundary, .entry.goal, .entry.checkpoint, .entry.unknown { font-size: .95rem; }
.part { margin: .6rem .75rem; background: var(--side); }
.part:last-child { margin-bottom: .75rem; }
.part .name { padding: .35rem .75rem 0; font-size: .75rem; color: var(--dim); font-family: ui-monospace, monospace; }
.part.thinking .name { color: var(--think); font-style: italic; }
.part.result.error { border-color: #b3453f; }
.part.result.error .name { color: #b3453f; }
.part.call summary { padding: .4rem .75rem; cursor: pointer; font-size: .75rem; color: var(--tool); font-family: ui-monospace, monospace; }
.part.call pre { border-top: 1px solid var(--line); }
.part.image pre { color: var(--dim); }
footer { margin-top: 2rem; color: var(--dim); font-size: .75rem; text-align: center; }
@media print { body { background: #fff; color: #000; } .entry { break-inside: avoid; } }
</style>
</head>
<body>
<header class="meta">
<h1>{{if .Title}}{{.Title}}{{else}}xdev session{{end}}</h1>
<dl>
<div><dt>session</dt><dd><code>{{.SessionID}}</code></dd></div>
{{if .Model}}<div><dt>model</dt><dd><code>{{.Model}}</code></dd></div>{{end}}
<div><dt>cwd</dt><dd><code>{{.CWD}}</code></dd></div>
{{if .Created}}<div><dt>created</dt><dd>{{.Created}}</dd></div>{{end}}
<div><dt>exported</dt><dd>{{.Exported}}</dd></div>
{{if .LeafID}}<div><dt>leaf</dt><dd><code>{{.LeafID}}</code></dd></div>{{end}}
<div><dt>entries</dt><dd>{{len .Entries}}</dd></div>
{{if .Source}}<div><dt>source</dt><dd><code>{{.Source}}</code></dd></div>{{end}}
</dl>
</header>
{{if .SystemPrompt}}<section class="system">
<h2>System prompt</h2>
<pre>{{.SystemPrompt}}</pre>
</section>
{{end}}<main>
{{range .Entries}}<article class="entry {{.Kind}}" data-kind="{{.Kind}}">
<header><span class="kind">{{.Title}}</span>{{if .Timestamp}}<time>{{.Timestamp}}</time>{{end}}{{if .Note}}<span class="note">{{.Note}}</span>{{end}}</header>
{{range .Parts}}{{if eq .Kind "tool_call"}}<details class="part call"><summary>{{if .Name}}{{.Name}}{{else}}tool call{{end}}{{if .CallID}} <code>{{.CallID}}</code>{{end}}</summary><pre>{{.Text}}</pre></details>
{{else if eq .Kind "tool_result"}}<div class="part result{{if .IsError}} error{{end}}">{{if .Name}}<div class="name">{{.Name}}{{if .IsError}} — error{{end}}</div>{{end}}<pre>{{.Text}}</pre></div>
{{else if eq .Kind "thinking"}}<div class="part thinking"><div class="name">thinking</div><pre>{{.Text}}</pre></div>
{{else if eq .Kind "image"}}<div class="part image"><pre>{{.Text}}</pre></div>
{{else if eq .Kind "data"}}<div class="part data"><pre>{{.Text}}</pre></div>
{{else}}<div class="part text"><pre>{{.Text}}</pre></div>
{{end}}{{end}}</article>
{{end}}</main>
<footer>exported by xdev · session <code>{{.SessionID}}</code></footer>
</body>
</html>
`))
