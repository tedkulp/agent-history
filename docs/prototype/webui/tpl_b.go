// PROTOTYPE — throwaway. Variant B: three-pane reader (mail-client style).
// Left: Machine → Project tree, always visible. Middle: Session list for the
// selection, or search hits. Right: Transcript as a dense log, tool calls
// always expanded with the first lines of output shown and the rest lazy.
package main

const variantB = `
{{define "B"}}{{template "head" .}}
<style>
body{height:100vh;display:grid;grid-template-rows:auto 1fr;overflow:hidden}
.b-top{display:flex;gap:12px;align-items:center;padding:8px 14px;border-bottom:1px solid var(--line);background:var(--panel)}
.b-top .brand{font-weight:700;width:196px} .b-top input{flex:1;max-width:560px;padding:6px 10px;border:1px solid var(--line);border-radius:6px;background:var(--bg);color:var(--ink)}
.b-grid{display:grid;grid-template-columns:220px 340px 1fr;min-height:0}
.b-grid>*{overflow:auto;min-height:0}
.b-tree{border-right:1px solid var(--line);padding:10px 0;background:var(--panel)}
.b-tree a{display:flex;justify-content:space-between;padding:3px 14px;color:var(--ink);font-size:13px} .b-tree a:hover{background:var(--acc-bg);text-decoration:none}
.b-tree a.on{background:var(--acc);color:#fff} .b-tree a.on .dim{color:#fffc}
.b-tree .mach{font-weight:600;margin-top:8px} .b-tree .proj{padding-left:28px}
.b-list{border-right:1px solid var(--line)}
.b-list h3{margin:0;padding:10px 14px;font-size:12px;color:var(--dim);font-weight:500;border-bottom:1px solid var(--line);position:sticky;top:0;background:var(--bg)}
.b-item{display:block;padding:9px 14px;border-bottom:1px solid var(--line);color:var(--ink)} .b-item:hover{background:var(--acc-bg);text-decoration:none}
.b-item.on{background:var(--acc-bg);box-shadow:inset 3px 0 var(--acc)}
.b-item .t{font-weight:500;display:block} .b-item .m{font-size:12px;color:var(--dim);display:flex;gap:6px;align-items:center}
.b-item .snip{font-size:12.5px;display:block;margin-top:2px}
.b-read{padding:0 0 120px}
.b-head{position:sticky;top:0;background:var(--bg);padding:10px 18px;border-bottom:1px solid var(--line);z-index:2}
.b-head h2{margin:0;font-size:16px} .b-head .m{font-size:12px;color:var(--dim);display:flex;flex-wrap:wrap;gap:4px 12px}
.b-row{display:grid;grid-template-columns:44px 64px 1fr;gap:8px;padding:4px 18px;scroll-margin-top:70px}
.b-row:hover{background:var(--panel)} .b-row .tm{font:11px var(--mono);color:var(--dim);padding-top:3px}
.b-row .who{font-size:11px;font-weight:600;text-transform:uppercase;padding-top:3px;color:var(--dim)} .b-row.user .who{color:var(--acc)}
.b-row.user{background:var(--user);margin-top:8px}
.b-tool{font:12.5px var(--mono);border-left:2px solid var(--line);padding:2px 0 2px 10px;margin:4px 0}
.b-tool .cmd{color:var(--ink)} .b-tool .cmd b{color:var(--acc)} .b-tool pre.out{max-height:none;border:0;background:none;padding:2px 0;color:var(--dim)}
.b-tool .more{font-size:12px;color:var(--acc);cursor:pointer}
.b-think{color:var(--dim);font-style:italic;font-size:13px} .b-think summary{cursor:pointer;list-style:none;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.b-marker{color:var(--dim);font-size:12px;border-top:1px dashed var(--line);border-bottom:1px dashed var(--line);padding:3px 18px;margin:8px 0}
.b-empty{display:grid;place-items:center;height:100%;color:var(--dim)}
</style>
<body>
<header class="b-top"><span class="brand">Agent History</span>
<form style="flex:1;display:flex"><input type="hidden" name="v" value="B">
{{if .Machine}}<input type="hidden" name="machine" value="{{.Machine}}">{{end}}{{if .Project}}<input type="hidden" name="project" value="{{.Project}}">{{end}}
<input name="q" value="{{.Q}}" placeholder="Search {{if .Project}}in {{projLabel .Project}}{{else if .Machine}}on {{(mach .Machine).Name}}{{else}}all Machines{{end}}…"></form></header>
<div class="b-grid">
<nav class="b-tree">
  <a href="{{u .V "q" .Q}}" class="{{if not .Machine}}on{{end}}"><span>All Machines</span><span class="dim">{{len (top (filter ""))}}</span></a>
  {{range machines}}{{$mid := .ID}}
    <a class="mach {{if and (eq $.Machine .ID) (not $.Project)}}on{{end}}" href="{{u $.V "machine" .ID "q" $.Q}}"><span>{{.Name}}</span><span class="dim small">{{ago .LastSeen}}</span></a>
    {{range projects .ID}}<a class="proj {{if and (eq $.Machine $mid) (eq $.Project .Key)}}on{{end}}" href="{{u $.V "machine" $mid "project" .Key "q" $.Q}}"><span>{{.Name}}</span><span class="dim">{{.Sessions}}</span></a>{{end}}
  {{end}}
</nav>
<section class="b-list">
{{if .Q}}{{$hits := search .Q .F}}
  <h3>{{len $hits}} hits for “{{.Q}}” · <a href="{{u .V "machine" .Machine "project" .Project}}">clear</a></h3>
  {{range $hits}}{{$s := .Session}}
  <a class="b-item {{if and $.Session (eq $.Session.ID $s.ID)}}on{{end}}" href="{{u $.V "machine" $.Machine "project" $.Project "q" $.Q "hl" $.Q "session" $s.ID}}{{with .Message}}#m-{{.ID}}{{end}}">
    <span class="t">{{hl $s.Title $.Q}}</span>
    <span class="m"><span class="badge {{$s.Source}}">{{$s.Source}}</span>{{(mach $s.MachineID).Name}} › {{projName $s}}{{with .Message}} · {{.Role}} {{clock .Time}}{{end}}</span>
    {{if .Message}}<span class="snip">{{snippet .Text $.Q}}</span>{{end}}
  </a>{{end}}
{{else}}{{$ss := top .F}}
  <h3>{{len $ss}} Sessions{{if .Project}} in {{projLabel .Project}}{{else if .Machine}} on {{(mach .Machine).Name}}{{end}}</h3>
  {{range $ss}}<a class="b-item {{if and $.Session (eq $.Session.ID .ID)}}on{{end}}" href="{{u $.V "machine" $.Machine "project" $.Project "session" .ID}}">
    <span class="t">{{.Title}}</span>
    <span class="m"><span class="badge {{.Source}}">{{.Source}}</span>{{if not $.Machine}}{{(mach .MachineID).Name}} › {{end}}{{if not $.Project}}{{projName .}} · {{end}}{{len .Messages}} msgs · {{ago .Last}}</span>
  </a>{{end}}
{{end}}
</section>
<article class="b-read">
{{with .Session}}{{$s := .}}
  <div class="b-head"><h2>{{.Title}}</h2>
  <div class="m"><span class="badge {{.Source}}">{{.Source}}</span><span>{{(mach .MachineID).Name}}</span><span>{{.Cwd}}</span>{{with .Branch}}<span>⎇ {{.}}</span>{{end}}<span>{{.Model}}</span><span>{{stamp .Started}}</span><span>{{toolCount .}} tool calls</span>{{with .Unknown}}<span class="status-error">⚠ {{.}} unknown</span>{{end}}
  {{with .ParentID}}{{$p := sess .}}<span>↰ child of <a href="{{u $.V "machine" $.Machine "project" $.Project "session" $p.ID}}#{{$s.SpawnCallID}}">{{$p.Title}}</a></span>{{end}}</div></div>
  {{range .Messages}}{{$msg := .}}
    {{range $i, $p := .Parts}}
    {{if eq .Kind "marker"}}<div class="b-marker">⟡ {{.Text}}</div>
    {{else}}
    <div class="b-row {{$msg.Role}}" {{if eq $i 0}}id="m-{{$msg.ID}}"{{end}}>
      <span class="tm">{{if eq $i 0}}{{clock $msg.Time}}{{end}}</span><span class="who">{{if eq $i 0}}{{$msg.Role}}{{end}}</span>
      <div>
      {{if eq .Kind "text"}}{{mdhl .Text $.HL}}
      {{else if eq .Kind "thinking"}}<details class="b-think"><summary>💭 {{firstLine .Text 120}}</summary>{{md .Text}}</details>
      {{else if eq .Kind "unknown"}}<div class="unknown">⚠ unknown <code>{{.ToolName}}</code> {{.Text}}</div>
      {{else if eq .Kind "attachment"}}<span class="dim small">📎 {{.Text}}</span>
      {{else if eq .Kind "tool_call"}}
        <div class="b-tool" id="{{.ID}}">
          <div class="cmd"><b>{{.ToolName}}</b> {{if not .DiffFile}}{{firstLine .ToolInput 140}}{{end}} <span class="status-{{.Status}}">{{if eq .Status "ok"}}✓{{else}}✗ error{{end}}</span></div>
          {{if .DiffFile}}{{diff .}}{{else if .ToolOutput}}
            <pre class="out">{{head .ToolOutput 8}}</pre>
            {{with more .ToolOutput 8}}<div class="more" hx-get="/tool/{{$p.ID}}" hx-target="previous pre" hx-swap="outerHTML" hx-on::after-request="this.remove()">… {{.}} more lines ({{size $p}}) · show all</div>{{end}}
          {{end}}
          {{with .ChildID}}{{$c := sess .}}<div>↳ <a href="{{u $.V "machine" $.Machine "project" $.Project "session" $c.ID}}">Child Session: {{$c.Title}}</a></div>{{end}}
        </div>
      {{end}}
      </div>
    </div>
    {{end}}{{end}}
  {{end}}
{{else}}<div class="b-empty">Select a Session</div>{{end}}
</article>
</div>
{{template "switcher" .}}</body></html>{{end}}
`
