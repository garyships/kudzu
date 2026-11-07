//go:build !unix

package vfs

import "os"

// private: there are no writable mounts on these systems.
func private(os.FileInfo) bool { return false }
