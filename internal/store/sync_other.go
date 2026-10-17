//go:build !linux

package store

import (
	"os"
	"path/filepath"
	"sync"
)

// syncObjects makes the given object files, and their directory entries,
// durable: each file is fsynced (a few at a time), then each directory once.
func syncObjects(_ string, files map[string]bool) error {
	var mu sync.Mutex
	var firstErr error
	work := make(chan string)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range work {
				if err := syncFile(p); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
				}
			}
		}()
	}
	dirs := map[string]bool{}
	for p := range files {
		dirs[filepath.Dir(p)] = true
		work <- p
	}
	close(work)
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	for d := range dirs {
		if err := syncDir(d); err != nil {
			return err
		}
	}
	return nil
}

func syncFile(p string) error {
	f, err := os.Open(p)
	if os.IsNotExist(err) {
		return nil // collected meanwhile
	}
	if err != nil {
		return err
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
