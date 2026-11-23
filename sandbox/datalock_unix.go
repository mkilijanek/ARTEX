//go:build unix

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// The server holds a shared lock on <data>/.isolation/server.lock for its whole life; the offline
// ownership command takes the same lock exclusively and so refuses to run while a server (in this
// container or in another one sharing the volume) is up. Locks belong to the open file, so they go
// when the process does, and a crash leaves nothing to clean up.

// ErrServerRunning means a server holds the data directory.
var ErrServerRunning = errors.New("a server is running against this data directory")

var (
	heldMu sync.Mutex
	held   []*os.File // kept for the life of the process: closing the file drops the lock
)

func openServerLock(dataDir string) (*os.File, error) {
	dir, err := OpenServerDir(dataDir, 0o700, ".isolation")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	return dir.OpenFile("server.lock", os.O_RDWR|os.O_CREATE|oNoFollow, 0o600)
}

// HoldServerLock takes the shared lock and keeps it until the process ends. The server calls it
// at start (sandbox.Activate).
func HoldServerLock(dataDir string) error {
	f, err := openServerLock(dataDir)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		f.Close()
		return fmt.Errorf("lock %s: %w", filepath.Join(dataDir, ".isolation", "server.lock"), err)
	}
	heldMu.Lock()
	held = append(held, f)
	heldMu.Unlock()
	return nil
}

// LockDataDirExclusive takes the lock exclusively without waiting. It returns ErrServerRunning when
// a server holds it. The caller calls the returned function when done.
func LockDataDirExclusive(dataDir string) (release func(), err error) {
	f, err := openServerLock(dataDir)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrServerRunning
		}
		return nil, fmt.Errorf("lock the data directory: %w", err)
	}
	return func() { f.Close() }, nil
}
