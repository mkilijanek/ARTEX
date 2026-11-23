//go:build !unix

package sandbox

import "errors"

var ErrServerRunning = errors.New("a server is running against this data directory")

// HoldServerLock: there is no lock on this platform.
func HoldServerLock(string) error { return nil }

// LockDataDirExclusive: there is no lock on this platform; the offline command is not available.
func LockDataDirExclusive(string) (func(), error) {
	return nil, errors.New("the data directory lock needs a Unix host")
}
