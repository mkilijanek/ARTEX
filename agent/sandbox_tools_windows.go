//go:build windows

package agent

import (
	"os"
	"os/exec"
	"time"
)

// The drop chain needs a Unix host, so the helper is never started here; these keep the package
// building.
func isolateProcess(cmd *exec.Cmd) { cmd.WaitDelay = 2 * time.Second }

func ownerUID(os.FileInfo) (uint32, bool) { return 0, false }
