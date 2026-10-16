//go:build unix

package vfs

import (
	"os"
	"syscall"
)

// ownedByMe reports whether path belongs to the current user. Metadata found
// by walking up from the current directory could have been planted by
// someone else (in /tmp, say) to point the kudzu command, and its token, at
// their own server, so only the user's own is trusted.
func ownedByMe(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}
