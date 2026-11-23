//go:build linux

package sandbox

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// chownEntry hands name, an entry of the directory dir (the same directory as r), to uid:gid. The
// entry is opened relative to dir as a path descriptor without following a link (a link is opened
// as itself, and nothing is read or started by the open), compared with what the walk saw, and
// changed through that descriptor, never by name. errEntryChanged: the name now leads to another
// file; errEntryHardLinked: the file got a second name since the walk looked.
func chownEntry(dir *os.File, _ *os.Root, name string, seen os.FileInfo, uid, gid int) error {
	want, ok := seen.Sys().(*syscall.Stat_t)
	if !ok {
		return errEntryChanged
	}
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return &os.PathError{Op: "openat", Path: name, Err: err}
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return &os.PathError{Op: "fstat", Path: name, Err: err}
	}
	if uint64(st.Dev) != uint64(want.Dev) || uint64(st.Ino) != uint64(want.Ino) {
		return errEntryChanged
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR && st.Nlink > 1 {
		return errEntryHardLinked
	}
	if err := unix.Fchownat(fd, "", uid, gid, unix.AT_EMPTY_PATH); err != nil {
		return &os.PathError{Op: "fchownat", Path: name, Err: err}
	}
	return nil
}
