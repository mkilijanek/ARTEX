//go:build !unix

package traffic

// noFollow: no such flag here; os.Root still refuses any path that leaves the root.
const noFollow = 0
