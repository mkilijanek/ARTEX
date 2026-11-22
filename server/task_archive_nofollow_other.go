//go:build !unix

package server

import "io/fs"

// archiveOpenFlags has no no-follow flag on this platform; os.Root still refuses any
// path that leaves the root, and the descriptor recheck still applies.
const archiveOpenFlags = 0

// archiveLinkCount: the link count is not available on this platform.
func archiveLinkCount(fs.FileInfo) (uint64, bool) { return 0, false }
