//go:build !unix

package proto

import (
	"context"
	"net"
)

// checkedDial returns dial as is: socket ownership is only checked on Unix.
func checkedDial(sock string, dial func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	return dial
}
