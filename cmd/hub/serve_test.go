package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/tedkulp/agent-history/internal/hub/store"
	"github.com/tedkulp/agent-history/protocol"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func waitHealthy(t *testing.T, addr string, errc <-chan error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-errc:
			t.Fatalf("serve exited early: %v", err)
		default:
		}
		if resp, err := http.Get("http://" + addr + "/healthz"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("hub never became healthy")
}

func TestSIGTERMStopsCleanlyAndKeepsAckedData(t *testing.T) {
	data, backups := t.TempDir(), t.TempDir()
	addr := freeAddr(t)
	errc := make(chan error, 1)
	go func() {
		errc <- serve([]string{"--listen", addr, "--data", data, "--backup-dir", backups, "--backup-at", ""})
	}()
	waitHealthy(t, addr, errc)

	const machine = "3f6c2a4e-8d1b-4f7a-9c2e-5b0d7e1a9f33"
	const key = "-Users-ted-src-app/3d34bfcc-90e7-4fd0-900f-86c047b22433.jsonl"
	body := []byte(`{"type":"user","uuid":"u1","parentUuid":null,"timestamp":"2026-09-01T10:00:00.000Z","cwd":"/x","message":{"role":"user","content":"hello"}}` + "\n")
	enc, _ := zstd.NewWriter(nil)
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+protocol.APIPrefix+"/machines/"+machine+"/records", bytes.NewReader(enc.EncodeAll(body, nil)))
	sum := sha256.Sum256(nil)
	req.Header.Set(protocol.HeaderMachineID, machine)
	req.Header.Set(protocol.HeaderSource, protocol.SourceClaudeCode)
	req.Header.Set(protocol.HeaderRecordKey, key)
	req.Header.Set(protocol.HeaderMode, protocol.ModeAppend)
	req.Header.Set(protocol.HeaderOffset, "0")
	req.Header.Set(protocol.HeaderPrefixSha256, hex.EncodeToString(sum[:]))
	req.Header.Set("Content-Encoding", protocol.ContentEncodingZstd)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ingest: %s", resp.Status)
	}

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("serve after SIGTERM: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not stop after SIGTERM")
	}
	if fi, err := os.Stat(filepath.Join(data, "hub.db-wal")); err == nil && fi.Size() != 0 {
		t.Errorf("WAL holds %d bytes after shutdown, want a checkpoint", fi.Size())
	}

	st, err := store.Open(context.Background(), data, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	got, err := st.CurrentContent(context.Background(), machine, protocol.SourceClaudeCode, key)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("after restart: %q, %v; want the acked chunk", got, err)
	}
}

func TestServeRefusesNewerDatabase(t *testing.T) {
	data := t.TempDir()
	st, err := store.Open(context.Background(), data, nil)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	// Bump user_version as a newer Hub's migration would.
	setUserVersion(t, filepath.Join(data, "hub.db"), 999)

	err = serve([]string{"--listen", freeAddr(t), "--data", data, "--backup-dir", t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "database is from a newer Hub") {
		t.Fatalf("serve = %v, want the newer-Hub error", err)
	}
}

func setUserVersion(t *testing.T, path string, v int) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, v)); err != nil {
		t.Fatal(err)
	}
}

func TestBackupCommandWritesTimestampedFile(t *testing.T) {
	data, backups := t.TempDir(), t.TempDir()
	st, err := store.Open(context.Background(), data, nil)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	if err := backupNow([]string{"--data", data, "--backup-dir", backups}); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(backups, "hub-????????-??????.db"))
	if len(files) != 1 {
		t.Fatalf("backups = %v", files)
	}
	db, err := sql.Open("sqlite", "file:"+files[0]+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var ok string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&ok); err != nil || ok != "ok" {
		t.Fatalf("integrity_check = %q, %v", ok, err)
	}
}
