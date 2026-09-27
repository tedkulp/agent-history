// Command notes prints the CHANGELOG.md section for the tag being released,
// the GitHub Release notes. The tag workflow runs it before goreleaser and
// fails when the section is missing: go run ./internal/release/notes v0.4.0
package main

import (
	"fmt"
	"os"

	"github.com/tedkulp/agent-history/internal/release"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: notes <tag>")
		os.Exit(2)
	}
	changelog, err := os.ReadFile("CHANGELOG.md")
	if err != nil {
		fmt.Fprintln(os.Stderr, "notes:", err)
		os.Exit(1)
	}
	notes, err := release.Notes(changelog, os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "notes:", err)
		os.Exit(1)
	}
	fmt.Print(notes)
}
