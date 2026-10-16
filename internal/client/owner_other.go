//go:build !unix

package client

import "os"

// fileOwner: ownership is not checked on these systems.
func fileOwner(os.FileInfo) (int, bool) { return 0, false }
