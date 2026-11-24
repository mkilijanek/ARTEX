//go:build linux

package notify

import (
	"context"
	"errors"
	"net"
	"strconv"

	"golang.org/x/sys/unix"
)

// signalDialSocket opens the path without following a symlink, checks that what it holds is a socket, and
// connects through /proc/self/fd/<n>, so the object that was checked is the object that is connected to. A
// path that is swapped for a symlink after the check cannot redirect the connection.
func signalDialSocket(ctx context.Context, socket string) (net.Conn, error) {
	fd, err := unix.Open(socket, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, errors.New(signalUnreachable)
		}
		return nil, errors.New("Signal service socket cannot be checked: " + err.Error())
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, errors.New("Signal service socket cannot be checked: " + err.Error())
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFSOCK:
	case unix.S_IFLNK:
		return nil, errors.New("Signal service socket path is a symlink; refusing to connect")
	default:
		return nil, errors.New("Signal service socket path is not a socket; refusing to connect")
	}
	return signalDialPath(ctx, "/proc/self/fd/"+strconv.Itoa(fd))
}
