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

  // Match the way FTS5 did (web.termMarker): a match starts at a word, and
  // ends at one except for the last term, which is a prefix.
  var re = new RegExp("(?<![\\p{L}\\p{N}])(?:" + terms.map(function (t, i) {
    var alt = t.split(" ").map(function (w) { return w.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"); }).join("[^\\p{L}\\p{N}]+");
    return i < terms.length - 1 ? alt + "(?![\\p{L}\\p{N}])" : alt;
  }).join("|") + ")", "giu");

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

// Live Transcript (hub.md §4.7): when the Session is parsed again from live data,
// fetch the page's last Message and every later one, swap them in by id, and
// refresh the header and outline. Open <details> stay open, the scroll
// position stays put, and the page follows new Messages only when the reader
// was already at the bottom. If earlier Messages moved, offer a reload.
(function () {
  "use strict";
  var main = document.querySelector(".tr-main[data-events]");
  if (!main || !window.EventSource || !window.fetch) return;
  var outline = document.querySelector(".outline");
  var es = new EventSource(main.dataset.events);
  var busy = false, again = false, gone = false;

  // A parse can land between the page render and the stream opening, or
  // while the stream was reconnecting, so catch up whenever it (re)opens.
  es.addEventListener("open", update);
  es.addEventListener("changed", update);

  function update() {
    if (gone) return;
    if (busy) { again = true; return; }
    busy = true;
    var url = main.dataset.messages + "?after=" + encodeURIComponent(main.dataset.after) + "&count=" + main.dataset.count;
    fetch(url, { headers: { "HX-Request": "true" } }).then(function (r) {
      if (r.status === 409) { stale(); return; }
      if (!r.ok) throw new Error("status " + r.status);
      return r.text().then(apply);
    }).catch(function (err) {
      console.warn("live update failed:", err);
    }).finally(function () {
      busy = false;
      if (again) { again = false; update(); }
    });
  }

  function apply(html) {
    var bottom = window.innerHeight + window.scrollY >= document.documentElement.scrollHeight - 40;
    var tpl = document.createElement("template");
    tpl.innerHTML = html;
    var u = tpl.content.querySelector(".tr-update");
    if (!u) return;

    swap(main.querySelector(".head"), u.querySelector(".head"));
    var list = main.querySelector(".tr-msgs");
    u.querySelectorAll(".tr-msgs > .msg").forEach(function (m) {
      var old = document.getElementById(m.id);
      if (old) swap(old, m);
      else { list.appendChild(m); htmx.process(m); }
    });
    if (outline) {
      swap(outline.querySelector(".outline-children"), u.querySelector(".outline-children"));
      var prompts = outline.querySelector(".prompts");
      u.querySelectorAll(".prompts > a").forEach(function (a) {
        var old = prompts.querySelector('a[data-m="' + CSS.escape(a.dataset.m) + '"]');
        if (old) old.replaceWith(a);
        else prompts.appendChild(a);
      });
    }
    main.dataset.after = u.dataset.after;
    main.dataset.count = u.dataset.count;
    if (bottom) window.scrollTo(0, document.documentElement.scrollHeight);
  }

  // swap replaces old with its re-rendered copy, keeping which <details> are
  // open: a tool row by its id, any other by its place among the id-less ones.
  function swap(old, fresh) {
    if (!old || !fresh) return;
    var open = {};
    keyed(old).forEach(function (e) { open[e.k] = e.d.open; });
    keyed(fresh).forEach(function (e) { if (e.k in open) e.d.open = open[e.k]; });
    old.replaceWith(fresh);
    htmx.process(fresh);
  }

  function keyed(root) {
    var n = 0;
    return Array.prototype.map.call(root.querySelectorAll("details"), function (d) {
      return { d: d, k: d.id ? "#" + d.id : String(n++) };
    });
  }

  function stale() {
    gone = true;
    es.close();
    if (document.querySelector(".banner.stale")) return;
    var b = document.createElement("div");
    b.className = "banner stale";
    b.append("Transcript changed — ");
    var a = document.createElement("a");
    a.href = location.pathname + location.search;
    a.textContent = "reload";
    b.appendChild(a);
    document.body.appendChild(b);
  }
})();
