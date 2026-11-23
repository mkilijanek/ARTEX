package server

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestAskOperatorInTheImage is the image-level half of AC29, driven by the private harness: the
// harness builds this test binary, copies it into the running image, plants links as the tool user
// in a real task workspace, and runs this test as root against the image's data directory. It
// prints one line per case, ASK <case> <OK|REFUSED> <detail>, and decides nothing itself. It does
// nothing unless ARTEX_ISO_ASK_DATA names the data directory.
func TestAskOperatorInTheImage(t *testing.T) {
	data, task := os.Getenv("ARTEX_ISO_ASK_DATA"), os.Getenv("ARTEX_ISO_ASK_TASK")
	if data == "" || task == "" {
		t.Skip("harness only: ARTEX_ISO_ASK_DATA and ARTEX_ISO_ASK_TASK")
	}
	ws := filepath.Join(data, "tasks", task)
	stage := func(name, src string) {
		att, err := stageAskOperatorImage(data, task, "", src)
		if err != nil {
			fmt.Printf("ASK %s REFUSED %v\n", name, err)
			return
		}
		fmt.Printf("ASK %s OK %s\n", name, att[0].Name)
	}
	stage("good", filepath.Join(ws, "shot.png"))
	stage("good2", filepath.Join(ws, "shot.png"))
	stage("link-to-key", filepath.Join(ws, "leak.png"))
	stage("link-to-bin", filepath.Join(ws, "bin.png"))
	stage("outside", os.Getenv("ARTEX_ISO_ASK_OUTSIDE"))
	stage("dotdot", filepath.Join(ws, "..", "..", "..", "..", "app", "artex.png"))
	// the browser MCP server's working directory under the tool home (a regular file is accepted, a
	// link planted there or a file elsewhere in the tool home is not)
	stage("browser", "/home/artex-tool/mcp/browser/shot.png")
	stage("browser-link", "/home/artex-tool/mcp/browser/leak.png")
	stage("tool-home", "/home/artex-tool/home.png")
}
