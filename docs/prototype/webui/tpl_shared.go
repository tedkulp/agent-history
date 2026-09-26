// PROTOTYPE — throwaway. Shared template bits: <head>, base CSS, tool output
// stub, and the floating variant switcher (not part of the design under test).
package main

const sharedTpl = `
{{define "head"}}<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Agent History · {{.V}} prototype</title>
<script src="https://cdn.jsdelivr.net/npm/htmx.org@2.0.4/dist/htmx.min.js"></script>
<style>
:root{--bg:#fbfbfa;--panel:#fff;--ink:#1d1d1f;--dim:#6b6b70;--line:#e4e4e2;--acc:#3b5bdb;--acc-bg:#edf1ff;
--user:#f1f4ff;--tool:#f6f6f4;--add:#e6f6ea;--add-ink:#1c6b33;--del:#fdecec;--del-ink:#9b2226;--warn:#fff4db;--mark:#ffe066;
--mono:ui-monospace,SFMono-Regular,Menlo,monospace;--sans:-apple-system,BlinkMacSystemFont,"Segoe UI",Inter,sans-serif}
@media (prefers-color-scheme:dark){:root{--bg:#141416;--panel:#1c1c1f;--ink:#e8e8ea;--dim:#9a9aa2;--line:#2e2e33;--acc:#8ea2ff;--acc-bg:#232a45;
--user:#1f2437;--tool:#202024;--add:#15301d;--add-ink:#8fe0a4;--del:#3a1a1c;--del-ink:#ffa3a8;--warn:#3a3016;--mark:#8a6d00}}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--ink);font:14px/1.5 var(--sans)}
a{color:var(--acc);text-decoration:none} a:hover{text-decoration:underline}
code{font:12.5px var(--mono);background:var(--tool);padding:1px 4px;border-radius:4px}
pre{font:12.5px/1.45 var(--mono);margin:0;white-space:pre-wrap;word-break:break-word}
pre.code,pre.out{background:var(--tool);border:1px solid var(--line);border-radius:6px;padding:8px 10px;max-height:480px;overflow:auto}
mark{background:var(--mark);color:inherit;border-radius:2px;padding:0 1px}
p{margin:.4em 0}
.dim{color:var(--dim)} .small{font-size:12px}
.badge{display:inline-block;font-size:11px;padding:0 6px;border-radius:9px;border:1px solid var(--line);color:var(--dim);white-space:nowrap}
.badge.claude-code{border-color:#d9773b;color:#d9773b}.badge.codex{border-color:#10a37f;color:#10a37f}
.badge.oh-my-pi{border-color:#a855f7;color:#a855f7}.badge.opencode{border-color:#3b82f6;color:#3b82f6}
.diff{border:1px solid var(--line);border-radius:6px;overflow:hidden;margin:4px 0}
.diff-file{font:12px var(--mono);padding:4px 8px;background:var(--tool);border-bottom:1px solid var(--line)}
.diff pre{padding:4px 0}.diff span{display:block;padding:0 8px}
.diff .add{background:var(--add);color:var(--add-ink)}.diff .del{background:var(--del);color:var(--del-ink)}
.new-file{font-size:11px;color:var(--add-ink)}
.stub{cursor:pointer;border:1px dashed var(--line);border-radius:6px;padding:6px 10px;color:var(--dim);font-size:12.5px}
.stub:hover{border-color:var(--acc);color:var(--acc)} .stub.htmx-request{opacity:.5}
.unknown{background:var(--warn);border-radius:6px;padding:6px 10px;font-size:12.5px}
.status-error{color:var(--del-ink)} .status-ok{color:var(--add-ink)}
:target{animation:flash 2s ease-out} @keyframes flash{from{background:var(--mark)}}
/* switcher */
#pswitch{position:fixed;bottom:16px;left:50%;transform:translateX(-50%);z-index:99;display:flex;align-items:center;gap:4px;
background:#111;color:#fff;border-radius:999px;padding:5px 6px;box-shadow:0 6px 24px #0006;font:13px var(--sans)}
#pswitch button{background:#333;color:#fff;border:0;border-radius:999px;width:28px;height:28px;cursor:pointer;font-size:14px}
#pswitch button:hover{background:#555} #pswitch .lbl{padding:0 10px;min-width:180px;text-align:center}
#pswitch .st{font:11px var(--mono);color:#aaa;padding-right:10px;max-width:40vw;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
</style></head>{{end}}

{{define "out"}}{{/* tool output: inline if small, lazy stub if big */}}
{{if .ToolOutput}}{{if big .}}<div class="stub" hx-get="/tool/{{.ID}}" hx-trigger="click" hx-swap="outerHTML">▸ Output collapsed · {{size .}} · click to load</div>
{{else}}<pre class="out">{{.ToolOutput}}</pre>{{end}}{{end}}{{end}}

{{define "switcher"}}
<div id="pswitch" title="PROTOTYPE variant switcher (← / →)">
<button id="pprev">←</button><span class="lbl"><b>{{.V}}</b> · {{.VName}}</span><button id="pnext">→</button>
<span class="st" id="pstate"></span></div>
<script>
(()=>{const keys={{variants}}.map(v=>v.Key);
const go=d=>{const u=new URL(location);const i=keys.indexOf(u.searchParams.get("v")||"A");
u.searchParams.set("v",keys[(i+d+keys.length)%keys.length]);location.replace(u)};
pprev.onclick=()=>go(-1);pnext.onclick=()=>go(1);
addEventListener("keydown",e=>{const t=e.target;if(t.closest("input,textarea,[contenteditable]"))return;
if(e.key==="ArrowLeft")go(-1);if(e.key==="ArrowRight")go(1)});
const p=new URL(location).searchParams;p.delete("v");pstate.textContent=[...p].map(([k,v])=>k+"="+v).join(" ")+(location.hash||"")||"(home)";})();
</script>{{end}}
`
