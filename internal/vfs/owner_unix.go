//go:build unix

package vfs

import (
	"os"
	"syscall"
)

// private reports whether fi belongs to the current user and nobody else
// can write to it. Metadata found by walking up from the current directory
// could have been planted by someone else (in /tmp, say) to point the kudzu
// command, and its token, at their own server, so only the user's own is
// trusted.
func private(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid() && fi.Mode().Perm()&0o022 == 0
}
