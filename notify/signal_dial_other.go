//go:build !linux

package notify

import (
	"context"
	"net"
)

// signalDialSocket checks the path and then connects to it. Without /proc the check and the connect are two
// steps, so a path swapped between them is not caught.
func signalDialSocket(ctx context.Context, socket string) (net.Conn, error) {
	if err := signalCheckSocket(socket); err != nil {
		return nil, err
	}
	return signalDialPath(ctx, socket)
}
