//go:build windows

package sandbox

import (
	"os"
	"time"
)

// The drop chain needs a Unix host; on Windows the isolation is reported unavailable and
// nothing here is used.
const oNoFollow = 0

const oNonblock = 0

func fileUID(os.FileInfo) (uint32, bool) { return 0, false }

func isNoProcess(error) bool { return false }

func killedBySIGKILL(error) bool { return false }

// fileMeta: the ownership walk needs a Unix host.
type fileMeta struct {
	UID, GID  uint32
	Nlink     uint64
	Changed   time.Time
	ChangedOK bool
}

func metaOf(os.FileInfo) (fileMeta, bool) { return fileMeta{}, false }
