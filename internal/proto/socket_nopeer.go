//go:build unix && !linux

package proto

import "net"

// peerUID: the syscall package offers no portable way to ask these systems
// who serves a unix socket, so only the socket file's owner is checked.
var peerUID = func(net.Conn) (int, error) { return 0, errNoPeerCred }
