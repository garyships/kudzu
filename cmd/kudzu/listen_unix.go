//go:build unix

package main

import (
	"net"
	"os"
	"syscall"
)

// listenUnix binds a unix socket that is no more open than mode even before
// listen's chmod: a connection accepted in between would outlive the chmod.
func listenUnix(sock string, mode os.FileMode) (net.Listener, error) {
	old := syscall.Umask(int(0o777 &^ mode.Perm()))
	defer syscall.Umask(old)
	return net.Listen("unix", sock)
}
