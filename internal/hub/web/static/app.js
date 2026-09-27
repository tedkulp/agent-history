// Arriving from search (hub.md §4.7): wrap each hl term in <mark> in the
// Transcript's text nodes, never tags or attributes, and open the collapsed
// rows of the target Message that hold a match. The target flashes by CSS.
(function () {
  "use strict";
  var hl = new URLSearchParams(location.search).get("hl");
  var main = document.querySelector(".tr-main");
  if (!hl || !main) return;

  // Split the query the way the Hub does (store.ParseQuery): words, with
  // "quoted phrases" kept together; FTS5 operator characters become spaces;
  // terms with no letter or digit are dropped.
  var terms = [], cur = "", quoted = false;
  function flush() {
    var t = cur.replace(/["*():^{}+\-]/g, " ").trim().split(/\s+/).join(" ");
    cur = "";
    if (/[\p{L}\p{N}]/u.test(t)) terms.push(t);
  }
  for (var ch of hl) {
    if (ch === '"') { flush(); quoted = !quoted; }
    else if (/\s/.test(ch) && !quoted) flush();
    else cur += ch;
  }
  flush();
  if (!terms.length) return;

  var re = new RegExp(terms.map(function (t) {
    return t.split(" ").map(function (w) { return w.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"); }).join("\\s+");
  }).join("|"), "giu");

  var nodes = [];
  main.querySelectorAll(".head h1, .msg").forEach(function (root) {
    var walk = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
    for (var n = walk.nextNode(); n; n = walk.nextNode()) nodes.push(n);
  });
  nodes.forEach(function (n) {
    var text = n.nodeValue, last = 0, frag = null, m;
    re.lastIndex = 0;
    while ((m = re.exec(text)) !== null) {
      if (!m[0]) { re.lastIndex++; continue; }
      frag = frag || document.createDocumentFragment();
      frag.appendChild(document.createTextNode(text.slice(last, m.index)));
      var mark = document.createElement("mark");
      mark.textContent = m[0];
      frag.appendChild(mark);
      last = m.index + m[0].length;
    }
    if (!frag) return;
    frag.appendChild(document.createTextNode(text.slice(last)));
    n.parentNode.replaceChild(frag, n);
  });

  var target = location.hash && document.getElementById(decodeURIComponent(location.hash.slice(1)));
  if (!target) return;
  target.querySelectorAll("mark").forEach(function (mk) {
    for (var d = mk.closest("details"); d && target.contains(d); d = d.parentElement.closest("details")) d.open = true;
  });
  target.scrollIntoView({ block: "center" });
})();
