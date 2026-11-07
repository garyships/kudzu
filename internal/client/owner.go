package client

import (
	"fmt"
	"os"
	"path/filepath"
)

// checkOwner refuses a working copy whose metadata belongs to another user,
// like git's "dubious ownership" check, or that other users can write to.
// Open walks up from the current directory, so a .kudzu planted in a shared
// directory such as /tmp could otherwise point the kudzu command, and the
// token it sends, at someone else's server. KUDZU_SAFE_DIRECTORY (a working
// copy path, or *) trusts a working copy anyway; root may use a working copy
// of the user it runs for under sudo.
func checkOwner(root string, meta os.FileInfo) error {
	owner, ok := fileOwner(meta)
	if !ok {
		return nil
	}
	if safe := os.Getenv("KUDZU_SAFE_DIRECTORY"); safe == "*" || (safe != "" && filepath.Clean(safe) == root) {
		return nil
	}
	if owner != os.Getuid() && !(os.Getuid() == 0 && os.Getenv("SUDO_UID") == fmt.Sprint(owner)) {
		return fmt.Errorf("refusing the working copy at %s: its %s directory belongs to user %d, not to you (if you trust it, set KUDZU_SAFE_DIRECTORY=%s)", root, metaDir, owner, root)
	}
	if meta.Mode().Perm()&0o022 != 0 {
		path := filepath.Join(root, metaDir)
		return fmt.Errorf("refusing the working copy at %s: other users can write to its %s directory (mode %s); chmod go-w %s fixes that", root, metaDir, meta.Mode().Perm(), path)
	}
	return nil
}
