package signalsidecar

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/mkilijanek/artex/db"
	"github.com/mkilijanek/artex/db/dbtest"
)

// sharedFakeBin is the stand-in for signal-cli, built once for the tests that
// need many children.
var sharedFakeBin string

// TestMain gives the package its own database name (the _signalsidecar suffix
// the guard knows), keeps the tests away from the operator's config file and
// fails a run that leaves a temporary database or role behind.
func TestMain(m *testing.M) {
	dbtest.MustIsolateFromConfigFile()
	db.SuffixTestDatabase("signalsidecar")
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	dir, err := os.MkdirTemp("", "fakecli-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "signalsidecar tests:", err)
		return 1
	}
	defer os.RemoveAll(dir)
	sharedFakeBin = filepath.Join(dir, "fakecli")
	if out, err := exec.Command("go", "build", "-o", sharedFakeBin, "./testdata/fakecli").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "signalsidecar tests: building the fake CLI: %v\n%s", err, out)
		return 1
	}
	code := m.Run()
	if err := dbtest.CheckNoLeftovers(); err != nil {
		fmt.Fprintln(os.Stderr, "signalsidecar tests:", err)
		if code == 0 {
			code = 1
		}
	}
	return code
}
