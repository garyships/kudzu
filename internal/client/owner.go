package client

import (
	"fmt"
	"os"
	"path/filepath"
)

// checkOwner refuses a working copy whose metadata belongs to another user,
// like git's "dubious ownership" check. Open walks up from the current
// directory, so a .kudzu planted in a shared directory such as /tmp could
// otherwise point the kudzu command, and the token it sends, at someone
// else's server. KUDZU_SAFE_DIRECTORY (a working copy path, or *) trusts a
// working copy anyway; root may use a working copy of the user it runs for
// under sudo.
func checkOwner(root string, meta os.FileInfo) error {
	owner, ok := fileOwner(meta)
	if !ok || owner == os.Getuid() || (os.Getuid() == 0 && os.Getenv("SUDO_UID") == fmt.Sprint(owner)) {
		return nil
	}
	if safe := os.Getenv("KUDZU_SAFE_DIRECTORY"); safe == "*" || (safe != "" && filepath.Clean(safe) == root) {
		return nil
	}
	return fmt.Errorf("refusing the working copy at %s: its %s directory belongs to user %d, not to you (if you trust it, set KUDZU_SAFE_DIRECTORY=%s)", root, metaDir, owner, root)
}
