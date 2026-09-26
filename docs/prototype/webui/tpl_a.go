// PROTOTYPE — throwaway. Variant A: drill-down pages with breadcrumbs.
// Machines → Projects → Sessions → Transcript, one level per page. The
// Transcript reads like a document; every tool call is a one-line <details>,
// except Edits, whose diffs start open. Search is its own results page.
package main

const variantA = `
{{define "A"}}{{template "head" .}}
<style>
.a-top{display:flex;align-items:center;gap:16px;padding:10px 24px;border-bottom:1px solid var(--line);background:var(--panel);position:sticky;top:0;z-index:5}
.a-top .brand{font-weight:700;color:var(--ink)} .a-crumbs{flex:1;color:var(--dim)} .a-crumbs a{color:var(--dim)} .a-crumbs b{color:var(--ink);font-weight:500}
.a-top input{width:320px;padding:6px 10px;border:1px solid var(--line);border-radius:6px;background:var(--bg);color:var(--ink)}
.a-main{max-width:920px;margin:0 auto;padding:24px 24px 120px}
h1{font-size:22px;margin:0 0 4px} h2{font-size:15px;margin:24px 0 8px}
table{width:100%;border-collapse:collapse;background:var(--panel);border:1px solid var(--line);border-radius:8px;overflow:hidden}
th,td{text-align:left;padding:8px 12px;border-bottom:1px solid var(--line)} th{font-size:12px;color:var(--dim);font-weight:500}
tr:last-child td{border-bottom:0} tbody tr:hover{background:var(--acc-bg)}
.a-meta{display:flex;flex-wrap:wrap;gap:4px 14px;color:var(--dim);font-size:13px;margin-bottom:20px}
.a-msg{padding:10px 0 12px;border-top:1px solid var(--line);scroll-margin-top:60px}
.a-who{font-size:12px;font-weight:600;color:var(--dim);display:flex;gap:8px;align-items:baseline}
.a-who .anchor{opacity:0} .a-msg:hover .anchor{opacity:1}
.a-msg.user .a-body{background:var(--user);border-radius:8px;padding:6px 12px}
details.a-tool{border:1px solid var(--line);border-radius:6px;margin:6px 0;background:var(--panel)}
details.a-tool>summary{cursor:pointer;padding:4px 10px;font-size:13px;list-style:none;display:flex;gap:8px;align-items:baseline}
details.a-tool>summary::before{content:"▸";color:var(--dim)} details.a-tool[open]>summary::before{content:"▾"}
details.a-tool>summary code{background:none;color:var(--dim);overflow:hidden;text-overflow:ellipsis;white-space:nowrap;flex:1}
details.a-tool>.body{padding:4px 10px 10px;display:grid;gap:6px}
details.a-think{color:var(--dim);font-size:13px;margin:4px 0} details.a-think summary{cursor:pointer}
.a-marker{text-align:center;color:var(--dim);font-size:12px;margin:14px 0;display:flex;gap:10px;align-items:center}
.a-marker::before,.a-marker::after{content:"";flex:1;border-top:1px dashed var(--line)}
.a-hitgroup{background:var(--panel);border:1px solid var(--line);border-radius:8px;padding:10px 14px;margin-bottom:12px}
.a-hit{display:block;padding:4px 0 4px 12px;border-left:2px solid var(--line);margin-top:6px;color:var(--ink)} .a-hit:hover{border-color:var(--acc);text-decoration:none}
.a-child{background:var(--acc-bg);border-radius:6px;padding:6px 10px;font-size:13px}
</style>
<body>
<header class="a-top">
  <a class="brand" href="{{u .V}}">Agent History</a>
  <nav class="a-crumbs">
    {{if or .Machine .Session .Q}}<a href="{{u .V}}">Machines</a>{{end}}
    {{with .Session}}{{$m := mach .MachineID}} › <a href="{{u $.V "machine" .MachineID}}">{{$m.Name}}</a> › <a href="{{u $.V "machine" .MachineID "project" (projKey .)}}">{{projName .}}</a>
      {{with .ParentID}}{{$p := sess .}} › <a href="{{u $.V "session" $p.ID}}">{{firstLine $p.Title 30}}</a>{{end}} › <b>{{firstLine .Title 40}}</b>
    {{else}}{{if .Machine}}{{$m := mach .Machine}} › {{if .Project}}<a href="{{u .V "machine" .Machine}}">{{$m.Name}}</a> › <b>{{projLabel .Project}}</b>{{else}}<b>{{$m.Name}}</b>{{end}}{{end}}
    {{if .Q}} › <b>Search</b>{{end}}{{end}}
  </nav>
  <form><input type="hidden" name="v" value="A"><input name="q" value="{{.Q}}" placeholder="Search all Transcripts…"></form>
</header>
<main class="a-main">
{{if .Q}}{{template "A-search" .}}
{{else if .Session}}{{template "A-transcript" .}}
{{else if and .Machine .Project}}
  {{$m := mach .Machine}}<h1>{{projLabel .Project}}</h1><div class="a-meta"><span>{{$m.Name}}</span><span>{{if ne .Project "-"}}{{.Project}}{{else}}Sessions started in the home or a temp directory{{end}}</span></div>
  <table><thead><tr><th>Session</th><th>Source</th><th>Branch</th><th>Messages</th><th>Last activity</th></tr></thead><tbody>
  {{range top .F}}<tr><td><a href="{{u $.V "session" .ID}}">{{.Title}}</a>{{with children .}} <span class="dim small">+{{len .}} child</span>{{end}}</td><td><span class="badge {{.Source}}">{{.Source}}</span></td>
  <td class="dim">{{.Branch}}</td><td class="dim">{{len .Messages}}</td><td class="dim">{{ago .Last}}</td></tr>{{end}}</tbody></table>
{{else if .Machine}}
  {{$m := mach .Machine}}<h1>{{$m.Name}}</h1><div class="a-meta"><span>{{$m.OS}}</span><span>home {{$m.Home}}</span><span>last seen {{ago $m.LastSeen}}</span></div>
  <table><thead><tr><th>Project</th><th>Path</th><th>Sessions</th><th>Last activity</th></tr></thead><tbody>
  {{range projects .Machine}}<tr><td><a href="{{u $.V "machine" $.Machine "project" .Key}}">{{.Name}}</a></td><td class="dim small">{{.Cwd}}</td><td>{{.Sessions}}</td><td class="dim">{{ago .Last}}</td></tr>{{end}}</tbody></table>
{{else}}
  <h1>Machines</h1><p class="dim">Pick a Machine to see its Projects.</p>
  <table><thead><tr><th>Machine</th><th>OS</th><th>Projects</th><th>Sessions</th><th>Last seen</th></tr></thead><tbody>
  {{range machines}}<tr><td><a href="{{u $.V "machine" .ID}}">{{.Name}}</a></td><td class="dim">{{.OS}}</td><td>{{len (projects .ID)}}</td><td>{{len (top (filter .ID))}}</td><td class="dim">{{ago .LastSeen}}</td></tr>{{end}}</tbody></table>
  <h2>Recent Sessions</h2>
  <table><tbody>{{range $i, $s := top .F}}{{if lt $i 5}}<tr><td><a href="{{u $.V "session" .ID}}">{{.Title}}</a></td><td><span class="badge {{.Source}}">{{.Source}}</span></td><td class="dim">{{(mach .MachineID).Name}} › {{projName .}}</td><td class="dim">{{ago .Last}}</td></tr>{{end}}{{end}}</tbody></table>
{{end}}
</main>
{{template "switcher" .}}</body></html>{{end}}

{{define "A-transcript"}}{{$s := .Session}}{{$m := mach $s.MachineID}}
<h1>{{$s.Title}}</h1>
<div class="a-meta">
  <span class="badge {{$s.Source}}">{{$s.Source}}</span><span>{{$m.Name}}</span><span title="{{$s.Cwd}}">{{$s.Cwd}}</span>
  {{with $s.Branch}}<span>⎇ {{.}}</span>{{end}}<span>{{$s.Model}}</span><span>{{stamp $s.Started}}</span><span>{{len $s.Messages}} messages</span>
  {{with $s.Unknown}}<span class="status-error">⚠ {{.}} unknown part(s)</span>{{end}}
</div>
{{with $s.ParentID}}{{$p := sess .}}<p class="a-child">↰ Child Session of <a href="{{u $.V "session" $p.ID}}#{{$s.SpawnCallID}}">{{$p.Title}}</a></p>{{end}}
{{range $s.Messages}}
<section class="a-msg {{.Role}}" id="m-{{.ID}}">
  <div class="a-who">{{if eq .Role "user"}}You{{else}}Assistant{{end}} <span>{{clock .Time}}</span> <a class="anchor" href="#m-{{.ID}}">#</a></div>
  <div class="a-body">
  {{range .Parts}}
    {{if eq .Kind "text"}}{{mdhl .Text $.HL}}
    {{else if eq .Kind "thinking"}}<details class="a-think"><summary>Thinking</summary>{{md .Text}}</details>
    {{else if eq .Kind "marker"}}<div class="a-marker">{{.Text}}</div>
    {{else if eq .Kind "unknown"}}<div class="unknown">⚠ Unknown <code>{{.ToolName}}</code> <pre>{{.Text}}</pre></div>
    {{else if eq .Kind "attachment"}}<div class="dim small">📎 {{.Text}}</div>
    {{else if eq .Kind "tool_call"}}
      <details class="a-tool" id="{{.ID}}" {{if .DiffFile}}open{{end}}>
        <summary><b>{{.ToolName}}</b><code>{{firstLine .ToolInput 90}}</code><span class="status-{{.Status}}">{{if eq .Status "ok"}}✓{{else}}✗{{end}}</span></summary>
        <div class="body">
          {{if .DiffFile}}{{diff .}}{{else}}<pre class="code">{{.ToolInput}}</pre>{{template "out" .}}{{end}}
          {{with .ChildID}}{{$c := sess .}}<div class="a-child">↳ Child Session: <a href="{{u $.V "session" $c.ID}}">{{$c.Title}}</a> · {{len $c.Messages}} messages</div>{{end}}
        </div>
      </details>
    {{end}}
  {{end}}
  </div>
</section>
{{end}}{{end}}

{{define "A-search"}}{{$hits := search .Q .F}}{{$groups := groupHits $hits}}
<h1>“{{.Q}}”</h1><p class="dim">{{len $hits}} matching Messages in {{len $groups}} Sessions, across all Machines. Tool output and thinking are not searched.</p>
{{range $groups}}{{$s := .Session}}
<div class="a-hitgroup">
  <div><a href="{{u $.V "session" $s.ID "hl" $.Q}}"><b>{{hl $s.Title $.Q}}</b></a></div>
  <div class="dim small"><span class="badge {{$s.Source}}">{{$s.Source}}</span> {{(mach $s.MachineID).Name}} › {{projName $s}} · {{ago $s.Last}}{{with $s.ParentID}} · child session{{end}}</div>
  {{range .Hits}}{{if .Message}}<a class="a-hit" href="{{u $.V "session" $s.ID "hl" $.Q}}#m-{{.Message.ID}}"><span class="dim small">{{.Message.Role}} · {{clock .Message.Time}}{{if eq .Field "tool"}} · tool call{{end}}</span><br>{{snippet .Text $.Q}}</a>{{end}}{{end}}
</div>
{{else}}<p>No matches.</p>{{end}}
{{end}}
`
