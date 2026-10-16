//go:build unix

package proto

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
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

// checkedDial wraps dial so that a connection to sock is only used if
// checkSocket accepts the socket. The check runs after connecting, so it
// sees the socket actually reached; nothing has been sent on it yet.
func checkedDial(sock string, dial func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
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
// one in a directory everyone may create files in (like /tmp) that is owned
// by neither this user nor root. Whoever binds such a path first receives
// every request meant for the real daemon, tokens and writes included.
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
	dir := filepath.Dir(path)
	if di, err := os.Stat(dir); err != nil || di.Mode().Perm()&0o002 == 0 {
		return nil
	}
	owner := "uid " + strconv.Itoa(uid)
	if u, err := user.LookupId(strconv.Itoa(uid)); err == nil {
		owner = fmt.Sprintf("%s (uid %d)", u.Username, uid)
	}
	return fmt.Errorf("refusing unix socket %s: it is owned by %s, not you, and any local user can create files in %s, so it may not be the daemon you expect; "+
		"if it is stale, have its owner or root remove it, or use a socket of your own (e.g. kudzu up --socket PATH); "+
		"a daemon shared between users should listen in a directory that is not world-writable", path, owner, dir)
}
