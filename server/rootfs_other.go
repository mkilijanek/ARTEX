//go:build !unix

package server

import "os"

// These platforms have no no-follow flag; os.Root still refuses any path that leaves the root.
const (
	noFollowFlag = 0
	nonBlockFlag = 0
)

func linkCount(os.FileInfo) (uint64, bool) { return 0, false }

func clearNonblock(*os.File) error { return nil }
