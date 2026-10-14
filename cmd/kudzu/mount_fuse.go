//go:build fuse

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/garyships/kudzu/internal/fusefs"
	"github.com/garyships/kudzu/internal/proto"
	"github.com/garyships/kudzu/internal/sparse"
	"github.com/garyships/kudzu/internal/store"
	"github.com/garyships/kudzu/internal/vfs"
)

// cmdMount presents a revision as a read-only filesystem whose files are
// fetched the first time they are read.
func cmdMount(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mount", flag.ContinueOnError)
	rev := fs.String("r", "main", "revision to mount")
	sparseDirs := fs.String("sparse", "", "comma-separated directories to show (default: everything)")
	cacheDir := fs.String("cache", "", "keep fetched objects in this directory (default: in memory; a node daemon already caches them on disk)")
	maxCache := fs.String("max-cache", "2GB", "with --cache: disk budget")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return errors.New("usage: kudzu mount [-r REV] [--sparse a,b] REMOTE MOUNTPOINT")
	}
	sp, err := sparse.Normalize(splitList(*sparseDirs))
	if err != nil {
		return err
	}
	rem, err := proto.Dial(pos[0])
	if err != nil {
		return err
	}
	var cache store.Objects
	if *cacheDir != "" {
		n, err := parseSize(*maxCache)
		if err != nil {
			return err
		}
		if cache, err = store.OpenFS(*cacheDir, n); err != nil {
			return err
		}
	}
	v, err := vfs.Open(ctx, rem, *rev, sp, cache)
	if err != nil {
		return err
	}
	srv, err := fusefs.Mount(pos[1], v, log.New(os.Stderr, "kudzu-mount ", log.LstdFlags))
	if err != nil {
		return fmt.Errorf("mount %s: %w", pos[1], err)
	}
	fmt.Printf("mounted %s (%s) read-only at %s; Ctrl-C to unmount\n", *rev, v.Changeset.Short(), pos[1])
	go func() {
		<-ctx.Done()
		srv.Unmount()
	}()
	srv.Wait()
	st := v.Stats()
	fmt.Printf("\nunmounted; fetched %s and %s (%s)\n", plural(int(st.TreesFetched), "tree", "trees"), plural(int(st.BlobsFetched), "file", "files"), formatBytes(st.BytesFetched))
	return nil
}
