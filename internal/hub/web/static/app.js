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

// In-page links: a same-page # link jumps without a history entry, so Back
// leaves the page rather than stepping through every jump. The address bar
// still shows the anchor. replaceState doesn't move :target, so the jumped-to
// element carries .is-target instead (the live Transcript carries it onto
// re-rendered copies). Modified clicks and links whose own handler already
// acted (↑ Top) are left alone.
(function () {
  "use strict";

  function byHash(hash) {
    if (hash.length < 2) return null;
    try { return document.getElementById(decodeURIComponent(hash.slice(1))); }
    catch (err) { return null; }
  }

  // setTarget moves .is-target, restarting its flash when the same element is hit again.
  function setTarget(el) {
    document.querySelectorAll(".is-target").forEach(function (t) { t.classList.remove("is-target"); });
    if (!el) return;
    void el.offsetWidth;
    el.classList.add("is-target");
  }

  function openAncestors(el) {
    for (var d = el.parentElement && el.parentElement.closest("details"); d; d = d.parentElement && d.parentElement.closest("details")) d.open = true;
  }

  // On load the browser has scrolled already, unless the target sat in a
  // collapsed row it didn't open.
  var initial = byHash(location.hash);
  if (initial) {
    var hidden = !initial.getClientRects().length;
    openAncestors(initial);
    if (hidden) initial.scrollIntoView({ block: "center" });
  }
  setTarget(initial);
  window.addEventListener("hashchange", function () { setTarget(byHash(location.hash)); });

  document.addEventListener("click", function (e) {
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    var a = e.target.closest('a[href^="#"]');
    if (!a) return;
    var hash = a.getAttribute("href"), el = byHash(hash);
    if (!el) return;
    e.preventDefault();
    openAncestors(el);
    el.scrollIntoView({ block: "center" });
    // Tab continues from the target, as it would after a real jump.
    if (!el.hasAttribute("tabindex")) el.setAttribute("tabindex", "-1");
    el.focus({ preventScroll: true });
    history.replaceState(history.state, "", hash);
    setTarget(el);
  });
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
  // open (a tool row by its id, any other by its place among the id-less
  // ones) and which element is the in-page target.
  function swap(old, fresh) {
    if (!old || !fresh) return;
    var open = {};
    keyed(old).forEach(function (e) { open[e.k] = e.d.open; });
    keyed(fresh).forEach(function (e) { if (e.k in open) e.d.open = open[e.k]; });
    var t = old.classList.contains("is-target") ? old : old.querySelector(".is-target");
    old.replaceWith(fresh);
    if (t && t.id) {
      var n = fresh.id === t.id ? fresh : fresh.querySelector("#" + CSS.escape(t.id));
      if (n) n.classList.add("is-target");
    }
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

// Live feed (hub.md §4.7): the first page of the feed hears which of its
// Sessions were parsed from live data, fetches just their rows and moves
// them to where they now sort, which is the top. Rows are never removed. The
// rows on screen stay put when rows land above them. After the stream drops
// and reopens, the first page's rows are fetched and merged the same way.
(function () {
  "use strict";
  var feed = document.querySelector(".feed[data-events]");
  if (!feed || !window.EventSource || !window.fetch) return;
  var es = new EventSource(feed.dataset.events);
  var opened = false, queue = Promise.resolve();

  es.addEventListener("open", function () {
    if (opened) fetchRows(null);
    opened = true;
  });
  es.addEventListener("changed", function (e) { fetchRows(e.data); });

  // fetchRows fetches the named Sessions' rows (null: the first page's) and
  // merges them, one fetch at a time so merges land in order.
  function fetchRows(ids) {
    var url = new URL(feed.dataset.rows, location.href);
    if (ids !== null) url.searchParams.set("ids", ids);
    queue = queue.then(function () {
      return fetch(url, { headers: { "HX-Request": "true" } }).then(function (r) {
        if (!r.ok) throw new Error("status " + r.status);
        return r.text();
      }).then(merge);
    }).catch(function (err) {
      console.warn("live feed update failed:", err);
    });
  }

  // after reports whether row a sorts after row b: older, the id breaking
  // ties, as the feed orders them.
  function after(a, b) {
    var d = Number(a.dataset.at) - Number(b.dataset.at);
    return d < 0 || (d === 0 && Number(a.dataset.id) < Number(b.dataset.id));
  }

  function merge(html) {
    var tpl = document.createElement("template");
    tpl.innerHTML = html;
    var fresh = Array.prototype.slice.call(tpl.content.querySelectorAll(".row[data-id]"));
    if (!fresh.length) return;
    var more = feed.querySelector(".more");
    var byId = new Map();
    fresh.forEach(function (r) { byId.set(r.dataset.id, r); });
    var kept = Array.prototype.filter.call(feed.querySelectorAll(".row[data-id]"), function (r) { return !byId.has(r.dataset.id); });
    // A row sorting past the last one kept belongs to a page "Load more"
    // hasn't fetched yet; it will come from there, so the page keeps its
    // current copy, if any.
    var last = kept[kept.length - 1];
    fresh = fresh.filter(function (r) {
      if (!(more && last && after(r, last))) return true;
      var old = feed.querySelector('.row[data-id="' + CSS.escape(r.dataset.id) + '"]');
      if (old) kept.push(old);
      return false;
    });

    var anchor = null, top = 0;
    if (feed.getBoundingClientRect().top < 0) {
      anchor = kept.find(function (r) { return r.getBoundingClientRect().bottom > 0; });
      if (anchor) top = anchor.getBoundingClientRect().top;
    }

    var rows = kept.concat(fresh).sort(function (a, b) { return after(a, b) ? 1 : after(b, a) ? -1 : 0; });
    feed.querySelectorAll(":scope > .day, :scope > .row, :scope > .empty").forEach(function (e) { e.remove(); });
    var frag = document.createDocumentFragment(), day = null;
    rows.forEach(function (r) {
      if (r.dataset.day !== day) {
        day = r.dataset.day;
        var h = document.createElement("div");
        h.className = "day";
        h.textContent = day;
        frag.appendChild(h);
      }
      frag.appendChild(r);
    });
    feed.insertBefore(frag, more);

    if (anchor) window.scrollBy(0, anchor.getBoundingClientRect().top - top);
  }
})();

// Back to top: the "↑ Top" link shows once the Transcript header has scrolled
// out of view. It scrolls up without a history entry and focuses the heading.
(function () {
  "use strict";
  var link = document.querySelector(".to-top");
  var main = document.querySelector(".tr-main");
  if (!link || !main) return;
  var queued = false;

  // The live Transcript swaps the header in place, so look it up each time.
  function showOrHide() {
    queued = false;
    var head = main.querySelector(".head");
    link.hidden = !head || head.getBoundingClientRect().bottom > 0;
  }
  function schedule() {
    if (!queued) { queued = true; requestAnimationFrame(showOrHide); }
  }
  window.addEventListener("scroll", schedule, { passive: true });
  window.addEventListener("resize", schedule);
  showOrHide();

  link.addEventListener("click", function (e) {
    if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    e.preventDefault();
    var reduceMotion = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
    window.scrollTo({ top: 0, behavior: reduceMotion ? "auto" : "smooth" });
    var h = document.getElementById("top");
    if (h) h.focus({ preventScroll: true });
  });
})();
