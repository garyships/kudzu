//go:build unix

package proto

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// socketOwner returns the uid that owns a file; tests replace it.
var socketOwner = func(fi os.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

// errNoPeerCred is peerUID's error where the system does not say who is
// at the other end of a unix socket.
var errNoPeerCred = errors.New("peer credentials are not available")

// checkedDial wraps dial so that a connection to sock is only used if
// checkSocket accepts the socket. The check runs after connecting, so it
// sees the socket actually reached; nothing has been sent on it yet.
func checkedDial(sock string, dial func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if strings.HasPrefix(sock, "@") {
			return nil, fmt.Errorf("refusing unix socket %s: an abstract socket has no owner or permissions, so any local user could have bound it; use a socket file", sock)
		}
		c, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		if err := checkSocket(sock); err != nil {
			c.Close()
			return nil, err
		}
		return c, nil
	}
}

// checkSocket refuses a socket that another local user could have planted:
// one owned by neither this user nor root, in a directory everyone may
// create files in (like /tmp) or in one of that user's own directories
// inside such a directory (like /tmp/x/). Whoever binds such a path first
// receives every request meant for the real daemon, tokens and writes
// included.
func checkSocket(sock string) error {
	if err := checkSocketEntry(sock); err != nil {
		return err
	}
	if real, err := filepath.EvalSymlinks(sock); err == nil && real != sock {
		return checkSocketEntry(real)
	}
	return nil
}

func checkSocketEntry(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil // nothing there to vouch for; the dial reports it
	}
	uid, ok := socketOwner(fi)
	if !ok || uid == 0 || uid == os.Getuid() {
		return nil
	}
	// Walk up through directories others own to one of this user's or
	// root's: if any on the way is world-writable, anyone could have made
	// what is below it. A daemon's own directory under /run is fine.
	dir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil
	}
	for {
		di, err := os.Stat(dir)
		if err != nil {
			return nil
		}
		if di.Mode().Perm()&0o002 != 0 {
			break
		}
		if o, ok := socketOwner(di); !ok || o == 0 || o == os.Getuid() || filepath.Dir(dir) == dir {
			return nil
		}
		dir = filepath.Dir(dir)
	}
	owner := "uid " + strconv.Itoa(uid)
	if u, err := user.LookupId(strconv.Itoa(uid)); err == nil {
		owner = fmt.Sprintf("%s (uid %d)", u.Username, uid)
	}
	return fmt.Errorf("refusing unix socket %s: it is owned by %s, not you, and any local user can create files in %s, so it may not be the daemon you expect; "+
		"if it is stale, have its owner or root remove it, or use a socket of your own (e.g. kudzu up --socket PATH); "+
		"a daemon shared between users should listen in a directory that is not world-writable, nor inside one", path, owner, dir)
}

// checkPrivateSocket refuses a connection c to sock unless both the socket
// and, where the system says, the process serving it are the current
// user's.
func checkPrivateSocket(sock string, c net.Conn) error {
	switch uid, err := peerUID(c); {
	case err == errNoPeerCred:
	case err != nil:
		return fmt.Errorf("refusing unix socket %s: cannot tell who serves it: %w", sock, err)
	case uid != os.Getuid():
		return fmt.Errorf("refusing unix socket %s: it is served by uid %d, not by you", sock, uid)
	}
	fi, err := os.Lstat(sock)
	if err != nil {
		return err
	}
	if uid, ok := socketOwner(fi); !ok || uid != os.Getuid() || fi.Mode().Type() != os.ModeSocket {
		return fmt.Errorf("refusing unix socket %s: it is not a socket of yours", sock)
	}
	return nil
}
