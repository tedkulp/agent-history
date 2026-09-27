// Command floorcheck fails when protocol.MinCollectorVersion is above the
// version of the tag being released. The tag workflow runs it before
// goreleaser: go run ./internal/release/floorcheck v0.4.0
package main

import (
	"fmt"
	"os"

	"github.com/tedkulp/agent-history/internal/release"
	"github.com/tedkulp/agent-history/protocol"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: floorcheck <tag>")
		os.Exit(2)
	}
	if err := release.CheckFloor(protocol.MinCollectorVersion, os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "floorcheck:", err)
		os.Exit(1)
	}
	fmt.Printf("floor %s is at most %s\n", protocol.MinCollectorVersion, os.Args[1])
}
