package main

import (
	"bytes"
	"flag"
	"strings"
	"testing"
)

func TestExecFlag(t *testing.T) {
	for _, name := range []string{"exec", "shim"} {
		fs := flag.NewFlagSet("service install", flag.ContinueOnError)
		exe := execFlag(fs)
		if err := fs.Parse([]string{"--" + name, "/opt/bin/agent-history"}); err != nil {
			t.Fatalf("--%s: %v", name, err)
		}
		if *exe != "/opt/bin/agent-history" {
			t.Fatalf("--%s: got %q", name, *exe)
		}
	}

	fs := flag.NewFlagSet("service install", flag.ContinueOnError)
	var out bytes.Buffer
	fs.SetOutput(&out)
	execFlag(fs)
	if err := fs.Parse([]string{"--help"}); err != flag.ErrHelp {
		t.Fatalf("--help: %v", err)
	}
	if !strings.Contains(out.String(), "-exec") || strings.Contains(out.String(), "-shim") {
		t.Fatalf("usage lists -exec and hides -shim, got:\n%s", out.String())
	}
}
