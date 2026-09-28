package runner

import (
	"bytes"
	"compress/gzip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tedkulp/agent-history/internal/collector/source/ohmypi"
	"github.com/tedkulp/agent-history/protocol"
)

const (
	ompSession = "-Users-ted-src-app/2026-09-17T17-58-38-183Z_01a0b085-1516-7000-9081-e596e495299d"
	ompKey     = ompSession + ".jsonl"
	ompChildK  = ompSession + "/T3SwitchResearch.jsonl"
	ompGrandK  = ompSession + "/T3SwitchResearch/T3SwitchResearch.T3ChatSwitch.jsonl"
	ompOtherK  = "-Users-ted-src-app/2026-09-18T09-00-00-000Z_01a0c000-1111-7222-8333-944455556666.jsonl"
)

// ompFile is a Session file: omp's fixed-width title slot, then the header.
func ompFile(title, cwd string) string {
	slot := `{"type":"title","v":1,"title":"` + title + `","updatedAt":"2026-09-17T18:13:24.506Z","pad":"`
	slot += strings.Repeat(" ", 120-len(slot)) + `"}` + "\n"
	return slot + `{"type":"session","version":3,"id":"01a0b085-1516-7000-9081-e596e495299d","timestamp":"2026-09-17T17:58:38.183Z","cwd":"` + cwd + `"}` + "\n"
}

// A Session file and its nested sub-agents ship under their keys; a title
// change ships as a replace; omp gc archiving ships nothing and makes no
// second record (oh-my-pi.md §6).
func TestOhMyPiSessionLifecycle(t *testing.T) {
	f := newFixture(t)
	body := ompFile("Plan the work", "/Users/ted/src/app")
	f.write("sessions/"+ompKey, body)
	f.write("sessions/"+ompChildK, ompFile("", "/Users/ted/src/app"))
	f.write("sessions/"+ompGrandK, ompFile("", "/Users/ted/src/app"))
	f.write("sessions/"+ompSession+"/T3SwitchResearch.md", "output")
	f.write("sessions/-Users-ted-src-app/.2026-09-17T17-58-38-183Z_01a0b085-1516-7000-9081-e596e495299d.jsonl.lock", "")

	cfg := f.config()
	cfg.Sources = []Source{{Adapter: ohmypi.Adapter{}, Root: f.root}}
	cfg.RescanInterval = 50 * time.Millisecond
	cfg.Control = NewControl()
	f.start(cfg)
	waitFor(t, 5*time.Second, "the Session files to ship", func() bool {
		return f.hubHolds(ompKey, body) && f.onHub(ompChildK) && f.onHub(ompGrandK)
	})

	rep := f.status(cfg.Control)
	if s := rep.Sources[0]; s.ID != protocol.SourceOhMyPi || s.Records != 3 || s.Ignored != 2 || s.Unclaimed != 0 {
		t.Errorf("status source %+v, unclaimed %q", s, s.UnclaimedPaths)
	}

	// omp rewrites the title slot in place, keeping its width.
	retitled := ompFile("Work planned", "/Users/ted/src/app")
	if len(retitled) != len(body) {
		t.Fatalf("slot width changed: %d vs %d", len(retitled), len(body))
	}
	posts := f.posts.Load()
	f.write("sessions/"+ompKey, retitled)
	waitFor(t, 5*time.Second, "the new title to ship", func() bool { return f.hubHolds(ompKey, retitled) })
	if f.posts.Load() == posts {
		t.Error("no records request carried the new title")
	}

	// omp gc gzips the Session into archive/ and moves its artifacts along.
	posts = f.posts.Load()
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	w.Write([]byte(retitled))
	w.Close()
	f.write("archive/sessions/"+ompKey+".gz", gz.String())
	if err := os.Remove(f.path("sessions/" + ompKey)); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(f.path("sessions/"+ompSession), f.path("archive/sessions/"+ompSession)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if n := f.posts.Load() - posts; n != 0 {
		t.Errorf("archiving sent %d records requests, want none", n)
	}
	if n := f.records(protocol.SourceOhMyPi); n != 3 || !f.hubHolds(ompKey, retitled) {
		t.Errorf("hub holds %d oh-my-pi records, current matches: %v", n, f.hubHolds(ompKey, retitled))
	}
}

// An excluded Session's sub-agents, nested ones too, stay on the Machine.
func TestOhMyPiExcludeCoversSubAgents(t *testing.T) {
	f := newFixture(t)
	f.write("sessions/"+ompKey, ompFile("", "/Users/ted/src/secret/app"))
	// A sub-agent's own cwd doesn't save it.
	f.write("sessions/"+ompChildK, ompFile("", "/Users/ted/src/app"))
	f.write("sessions/"+ompGrandK, ompFile("", "/Users/ted/src/app"))
	open := ompFile("", "/Users/ted/src/app")
	f.write("sessions/"+ompOtherK, open)

	cfg := f.config()
	cfg.Sources = []Source{{Adapter: ohmypi.Adapter{}, Root: f.root}}
	cfg.Exclude = excludeFilter(t)
	cfg.RescanInterval = 50 * time.Millisecond
	f.start(cfg)
	waitFor(t, 5*time.Second, "the open Session to ship", func() bool { return f.hubHolds(ompOtherK, open) })
	time.Sleep(200 * time.Millisecond)

	for _, k := range []string{ompKey, ompChildK, ompGrandK} {
		if f.onHub(k) {
			t.Errorf("excluded record %s reached the Hub", k)
		}
	}
	if n := cfg.Exclude.Count(protocol.SourceOhMyPi); n != 3 {
		t.Errorf("excluded count = %d, want 3", n)
	}
}
