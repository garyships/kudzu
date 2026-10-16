//go:build !unix

package vfs

// ownedByMe: there are no writable mounts on these systems.
func ownedByMe(string) bool { return false }
