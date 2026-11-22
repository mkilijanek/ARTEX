//go:build !linux

package sandbox

import (
	"os"
)

// chownEntry hands name, an entry of the directory r, to uid:gid. Without path descriptors only a
// regular file or a directory can be opened without side effects; it is opened relative to r
// without following a link, compared with what the walk saw, and changed through the descriptor,
// never by name. Any other type is refused with errEntryUnsupported.
func chownEntry(_ *os.File, r *os.Root, name string, seen os.FileInfo, uid, gid int) error {
	if !seen.Mode().IsRegular() && !seen.IsDir() {
		return errEntryUnsupported
	}
	f, err := r.OpenFile(name, os.O_RDONLY|oNoFollow|oNonblock, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(seen, fi) {
		return errEntryChanged
	}
	if m, ok := metaOf(fi); ok && !fi.IsDir() && m.Nlink > 1 {
		return errEntryHardLinked
	}
	return f.Chown(uid, gid)
}
