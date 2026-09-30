#!/usr/bin/env python3
"""Collect every Session with Parse warnings or a failed parse from a Hub's
web UI and print them as JSON, grouped by cause.

Usage: fetch_warnings.py <hub-url>

The Hub has no JSON endpoint for this (the /api/v1 routes need a Collector's
machine token), so this walks the "has warnings" feed (/?warnings=1, following
"Load more") and reads each Session page's warning notes.
"""

import html
import json
import re
import sys
import urllib.parse
import urllib.request


def get(url):
    with urllib.request.urlopen(url, timeout=30) as r:
        return r.read().decode("utf-8")


def text(s):
    return html.unescape(re.sub(r"<[^>]+>", "", s)).strip()


def session_ids(base):
    ids, url = [], base + "/?warnings=1"
    while url:
        page = get(url)
        for i in re.findall(r'<a class="row" href="/sessions/(\d+)"', page):
            if int(i) not in ids:
                ids.append(int(i))
        more = re.search(r'<a class="more" href="([^"]+)"', page)
        url = urllib.parse.urljoin(base + "/", html.unescape(more.group(1))) if more else None
    return ids


def session(base, sid):
    page = get(f"{base}/sessions/{sid}")
    head = re.search(r'<h1 id="top"[^>]*>(.*?)</h1>', page, re.S)
    badge = re.search(r'<span class="badge ([^"]+)">', page)
    out = {"id": sid, "url": f"{base}/sessions/{sid}",
           "title": text(head.group(1)) if head else "",
           "source": badge.group(1) if badge else "", "warnings": [], "parse_error": None}
    failed = re.search(r'<details class="notes failed">.*?<pre>(.*?)</pre>', page, re.S)
    if failed:
        out["parse_error"] = html.unescape(failed.group(1))
    notes = re.search(r'<details class="notes"><summary>.*?</summary>(.*?)</details>', page, re.S)
    for li in re.findall(r"<li>(.*?)</li>", notes.group(1), re.S) if notes else []:
        kind = re.search(r"<b>(.*?)</b>", li)
        st = re.search(r"<code>(.*?)</code>", li)
        count = re.search(r"×(\d+)", li)
        ver = re.search(r'<span class="dim">· (.*?)</span>', li, re.S)
        pre = re.search(r"<pre>(.*?)</pre>", li, re.S)
        out["warnings"].append({
            "kind": text(kind.group(1)) if kind else "",
            "source_type": text(st.group(1)) if st else "",
            "count": int(count.group(1)) if count else 0,
            "source_version": text(ver.group(1)) if ver else "",
            "excerpt": html.unescape(pre.group(1)) if pre else "",
        })
    return out


def main():
    if len(sys.argv) != 2:
        sys.exit("usage: fetch_warnings.py <hub-url>")
    base = sys.argv[1].rstrip("/")
    if "://" not in base:
        base = "http://" + base
    sessions = [session(base, i) for i in session_ids(base)]

    groups = {}
    for s in sessions:
        for w in s["warnings"]:
            key = (w["kind"], w["source_type"], w["source_version"])
            g = groups.setdefault(key, {"kind": w["kind"], "source_type": w["source_type"],
                                        "source_version": w["source_version"], "count": 0,
                                        "sessions": [], "excerpt": w["excerpt"]})
            g["count"] += w["count"]
            g["sessions"].append({"id": s["id"], "url": s["url"], "title": s["title"]})
    failures = [{"id": s["id"], "url": s["url"], "title": s["title"], "source": s["source"],
                 "parse_error": s["parse_error"]} for s in sessions if s["parse_error"]]
    json.dump({"hub": base, "sessions": len(sessions),
               "groups": sorted(groups.values(), key=lambda g: -len(g["sessions"])),
               "parse_failures": failures}, sys.stdout, indent=2, ensure_ascii=False)
    print()


if __name__ == "__main__":
    main()
