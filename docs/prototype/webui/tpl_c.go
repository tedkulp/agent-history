// PROTOTYPE — throwaway. Variant C: search-first feed.
// Home is a big search box over a day-grouped feed of recent Sessions from
// every Machine; Machine / Source / Project are filter chips, not pages.
// Transcript is chat bubbles; runs of tool calls fold into one cluster, and a
// sticky outline of your prompts sits on the left for jumping around.
package main

const variantC = `
{{define "C"}}{{template "head" .}}
<style>
.c-wrap{max-width:1100px;margin:0 auto;padding:18px 20px 120px}
.c-search{display:flex;gap:10px;align-items:center;margin-bottom:10px}
.c-search .brand{font-weight:700;white-space:nowrap}
.c-search input{flex:1;font-size:17px;padding:11px 16px;border:1px solid var(--line);border-radius:12px;background:var(--panel);color:var(--ink);box-shadow:0 1px 3px #0001}
.c-chips{display:flex;flex-wrap:wrap;gap:6px;margin-bottom:18px;font-size:12.5px;align-items:center}
.chip{border:1px solid var(--line);border-radius:999px;padding:2px 10px;color:var(--ink);background:var(--panel)} .chip:hover{border-color:var(--acc);text-decoration:none}
.chip.on{background:var(--acc);border-color:var(--acc);color:#fff} .chip .n{color:var(--dim)} .chip.on .n{color:#fffc}
.c-chips .lab{color:var(--dim);margin-left:8px}
.c-day{font-size:12px;font-weight:600;color:var(--dim);margin:18px 0 4px;text-transform:uppercase;letter-spacing:.04em}
.c-row{display:grid;grid-template-columns:52px 1fr auto;gap:12px;padding:8px 10px;border-radius:8px;color:var(--ink);align-items:baseline}
.c-row:hover{background:var(--panel);text-decoration:none;box-shadow:0 0 0 1px var(--line)}
.c-row .tm{font:12px var(--mono);color:var(--dim)} .c-row .t{font-weight:500} .c-row .p{font-size:12.5px;color:var(--dim);display:block;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.c-row .r{font-size:12px;color:var(--dim);text-align:right}
.c-results{display:grid;grid-template-columns:1fr 200px;gap:24px}
.c-facets h4{font-size:11px;text-transform:uppercase;color:var(--dim);margin:14px 0 4px}
.c-facets a{display:flex;justify-content:space-between;font-size:13px;padding:2px 0}
.c-hit{display:block;padding:10px 12px;border-radius:8px;color:var(--ink);border:1px solid transparent} .c-hit:hover{border-color:var(--line);background:var(--panel);text-decoration:none}
.c-hit .m{font-size:12px;color:var(--dim)}
.c-tr{display:grid;grid-template-columns:220px 1fr;gap:28px}
.c-outline{position:sticky;top:12px;align-self:start;max-height:calc(100vh - 90px);overflow:auto;font-size:12.5px;border-right:1px solid var(--line);padding-right:12px}
.c-outline a{display:block;padding:4px 0;color:var(--dim);border-left:2px solid transparent;padding-left:8px} .c-outline a:hover{color:var(--acc);border-color:var(--acc);text-decoration:none}
.c-outline .n{font-size:11px}
.c-head h1{font-size:20px;margin:0} .c-head .m{font-size:12.5px;color:var(--dim);display:flex;flex-wrap:wrap;gap:4px 12px;margin:4px 0 18px}
.c-msg{display:flex;flex-direction:column;margin:10px 0;scroll-margin-top:20px}
.c-msg.user{align-items:flex-end}
.bubble{max-width:78%;padding:8px 14px;border-radius:14px;background:var(--panel);border:1px solid var(--line)}
.c-msg.user .bubble{background:var(--acc);color:#fff;border-color:var(--acc);border-bottom-right-radius:4px}
.c-msg.user .bubble code{background:#fff3;color:#fff} .c-msg.user .bubble mark{color:#000}
.c-msg.assistant .bubble{border-bottom-left-radius:4px}
.c-time{font-size:11px;color:var(--dim);margin:2px 6px}
details.cluster{max-width:78%;font-size:13px;margin:3px 0;color:var(--dim)}
details.cluster>summary{cursor:pointer;padding:3px 10px;border-radius:999px;background:var(--tool);display:inline-block}
details.cluster[open]>summary{margin-bottom:4px}
details.ctool{border-left:2px solid var(--line);margin:2px 0 2px 10px;padding-left:10px}
details.ctool>summary{cursor:pointer;color:var(--ink)} details.ctool>div{display:grid;gap:4px;padding:4px 0}
details.cthink{font-size:12.5px;color:var(--dim);margin:3px 0} details.cthink summary{cursor:pointer}
.c-marker{align-self:center;font-size:12px;color:var(--dim);background:var(--tool);padding:2px 12px;border-radius:999px;margin:12px 0}
</style>
<body><div class="c-wrap">
<form class="c-search"><a class="brand" href="{{u .V}}">Agent History</a><input type="hidden" name="v" value="C">
{{if .Machine}}<input type="hidden" name="machine" value="{{.Machine}}">{{end}}{{if .Source}}<input type="hidden" name="source" value="{{.Source}}">{{end}}{{if .Project}}<input type="hidden" name="project" value="{{.Project}}">{{end}}
<input name="q" value="{{.Q}}" placeholder="Search every Transcript on every Machine…" {{if not .Session}}autofocus{{end}}></form>

{{if .Session}}{{template "C-transcript" .}}
{{else}}
<div class="c-chips">
  <span class="lab">Machine</span>{{range machines}}<a class="chip {{if eq $.Machine .ID}}on{{end}}" href="{{if eq $.Machine .ID}}{{u $.V "q" $.Q "source" $.Source}}{{else}}{{u $.V "machine" .ID "q" $.Q "source" $.Source}}{{end}}">{{.Name}}</a>{{end}}
  <span class="lab">Source</span>{{range sources}}<a class="chip {{if eq $.Source .}}on{{end}}" href="{{if eq $.Source .}}{{u $.V "q" $.Q "machine" $.Machine "project" $.Project}}{{else}}{{u $.V "source" . "q" $.Q "machine" $.Machine "project" $.Project}}{{end}}">{{.}}</a>{{end}}
  {{if .Machine}}<span class="lab">Project</span>{{range projects .Machine}}<a class="chip {{if eq $.Project .Key}}on{{end}}" href="{{if eq $.Project .Key}}{{u $.V "machine" $.Machine "q" $.Q "source" $.Source}}{{else}}{{u $.V "machine" $.Machine "project" .Key "q" $.Q "source" $.Source}}{{end}}">{{.Name}} <span class="n">{{.Sessions}}</span></a>{{end}}{{end}}
</div>
{{if .Q}}{{$hits := search .Q .F}}
<div class="c-results"><div>
  <p class="dim small">{{len $hits}} results · ranked by recency</p>
  {{range $hits}}{{$s := .Session}}
  <a class="c-hit" href="{{u $.V "session" $s.ID "hl" $.Q}}{{with .Message}}#m-{{.ID}}{{end}}">
    <div><b>{{hl $s.Title $.Q}}</b></div>
    {{if .Message}}<div>{{snippet .Text $.Q}}</div>{{end}}
    <div class="m"><span class="badge {{$s.Source}}">{{$s.Source}}</span> {{(mach $s.MachineID).Name}} › {{projName $s}} · {{with .Message}}{{.Role}} · {{end}}{{ago $s.Last}}</div>
  </a>{{else}}<p>No matches.</p>{{end}}
</div>
<aside class="c-facets">
  <h4>Machine</h4>{{range facets $hits "machine"}}<a href="{{u $.V "q" $.Q "machine" .Key "source" $.Source}}"><span>{{.Label}}</span><span class="dim">{{.N}}</span></a>{{end}}
  <h4>Source</h4>{{range facets $hits "source"}}<a href="{{u $.V "q" $.Q "source" .Key "machine" $.Machine}}"><span>{{.Label}}</span><span class="dim">{{.N}}</span></a>{{end}}
  <h4>Project</h4>{{range facets $hits "project"}}<span class="small" style="display:flex;justify-content:space-between"><span>{{.Label}}</span><span class="dim">{{.N}}</span></span>{{end}}
</aside></div>
{{else}}{{$ss := top .F}}
  {{range $i, $s := $ss}}{{$pv := prev $ss $i}}{{if or (not $pv) (not (sameDay $pv.Last $s.Last))}}<div class="c-day">{{day $s.Last}}</div>{{end}}
  <a class="c-row" href="{{u $.V "session" .ID}}"><span class="tm">{{clock .Last}}</span>
    <span><span class="t">{{.Title}}</span><span class="p">{{firstLine (first .) 140}}</span></span>
    <span class="r"><span class="badge {{.Source}}">{{.Source}}</span><br>{{(mach .MachineID).Name}} › {{projName .}}</span></a>
  {{end}}
{{end}}{{end}}
</div>{{template "switcher" .}}</body></html>{{end}}

{{define "C-transcript"}}{{$s := .Session}}
<div class="c-tr">
<nav class="c-outline">
  <div class="dim small" style="margin-bottom:6px">Your prompts</div>
  {{range $s.Messages}}{{if eq .Role "user"}}<a href="#m-{{.ID}}">{{range .Parts}}{{firstLine .Text 60}}{{end}} <span class="n">{{clock .Time}}</span></a>{{end}}{{end}}
  {{with children $s}}<div class="dim small" style="margin:10px 0 4px">Child Sessions</div>{{range .}}<a href="{{u $.V "session" .ID}}">↳ {{.Title}}</a>{{end}}{{end}}
</nav>
<div>
  <div class="c-head"><h1>{{$s.Title}}</h1>
  <div class="m"><span class="badge {{$s.Source}}">{{$s.Source}}</span><a href="{{u $.V "machine" $s.MachineID}}">{{(mach $s.MachineID).Name}}</a><a href="{{u $.V "machine" $s.MachineID "project" (projKey $s)}}">{{projName $s}}</a>{{with $s.Branch}}<span>⎇ {{.}}</span>{{end}}<span>{{$s.Model}}</span><span>{{stamp $s.Started}}</span>{{with $s.Unknown}}<span class="status-error">⚠ {{.}} unknown</span>{{end}}
  {{with $s.ParentID}}{{$p := sess .}}<span>↰ child of <a href="{{u $.V "session" $p.ID}}#{{$s.SpawnCallID}}">{{$p.Title}}</a></span>{{end}}</div></div>
  {{range $s.Messages}}{{$msg := .}}
  <div class="c-msg {{.Role}}" id="m-{{.ID}}">
    {{range chunks .Parts}}
      {{if .Tools}}
        <details class="cluster" {{range .Tools}}{{if .ChildID}}open{{end}}{{end}}><summary>⚙ {{len .Tools}} tool call{{if gt (len .Tools) 1}}s{{end}} · {{range $i, $t := .Tools}}{{if $i}}, {{end}}{{$t.ToolName}}{{if eq $t.Status "error"}} ✗{{end}}{{end}}</summary>
        {{range .Tools}}<details class="ctool" id="{{.ID}}"><summary><b>{{.ToolName}}</b> <span class="dim">{{firstLine .ToolInput 80}}</span> <span class="status-{{.Status}}">{{if eq .Status "ok"}}✓{{else}}✗{{end}}</span></summary>
          <div>{{if .DiffFile}}{{diff .}}{{else}}<pre class="code">{{.ToolInput}}</pre>{{template "out" .}}{{end}}</div></details>
          {{with .ChildID}}{{$c := sess .}}<div style="margin-left:22px">↳ <a href="{{u $.V "session" $c.ID}}">Child Session: {{$c.Title}}</a></div>{{end}}
        {{end}}
        </details>
      {{else}}{{with .Part}}
        {{if eq .Kind "text"}}<div class="bubble">{{mdhl .Text $.HL}}</div>
        {{else if eq .Kind "thinking"}}<details class="cthink"><summary>💭 thinking</summary>{{md .Text}}</details>
        {{else if eq .Kind "marker"}}<div class="c-marker">{{.Text}}</div>
        {{else if eq .Kind "unknown"}}<div class="unknown bubble">⚠ unknown <code>{{.ToolName}}</code><pre>{{.Text}}</pre></div>
        {{else if eq .Kind "attachment"}}<div class="dim small">📎 {{.Text}}</div>{{end}}
      {{end}}{{end}}
    {{end}}
    {{if ne (index .Parts 0).Kind "marker"}}<span class="c-time">{{clock .Time}}</span>{{end}}
  </div>
  {{end}}
</div></div>{{end}}
`
