//go:build !windows

package main

import (
	"fmt"
	"os"

	"github.com/mkilijanek/artex/agent"
)

// runToolfs is the file-tool helper: `artex __toolfs`, started through the drop chain, so it runs
// as the tool user. It reads one request from stdin and writes the result to stdout (see
// agent.RunToolfs). The server starts it per call; nothing else uses it.
func runToolfs(args []string) int {
	if len(args) != 0 {
		fmt.Fprintln(os.Stderr, "artex: __toolfs takes no arguments")
		return 64
	}
	if err := agent.RunToolfs(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "artex: "+err.Error())
		return 2
	}
	return 0
}
