//go:build !unix

package main

import (
	"net"
	"os"
)

func listenUnix(sock string, mode os.FileMode) (net.Listener, error) {
	return net.Listen("unix", sock)
}
