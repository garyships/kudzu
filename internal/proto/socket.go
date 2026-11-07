package proto

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
)

// DialPrivate is Dial for a unix socket that only the current user's own
// processes serve, such as a writable mount's control socket. It sends no
// token, and refuses the socket unless its path is absolute and it belongs
// to the current user, as does, where the system says, the process at the
// other end.
func DialPrivate(sock string) (*Remote, error) {
	if !filepath.IsAbs(sock) {
		return nil, fmt.Errorf("refusing unix socket %q: a private socket must have an absolute path", sock)
	}
	r, err := Dial("unix://" + sock)
	if err != nil {
		return nil, err
	}
	r.Token = ""
	tr, ok := r.hc.Transport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("unix socket %s: unexpected transport %T", sock, r.hc.Transport)
	}
	dial := tr.DialContext
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		if err := checkPrivateSocket(sock, c); err != nil {
			c.Close()
			return nil, err
		}
		return c, nil
	}
	return r, nil
}
