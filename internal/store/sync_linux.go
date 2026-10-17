package store

import (
	"os"

	"golang.org/x/sys/unix"
)

// syncObjects makes the given object files under dir, and their directory
// entries, durable. On Linux one syncfs(2) of the store's filesystem does it
// however many objects there are.
func syncObjects(dir string, _ map[string]bool) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return unix.Syncfs(int(d.Fd()))
}
