//go:build fuse

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/garyships/kudzu/internal/fusefs"
	"github.com/garyships/kudzu/internal/proto"
	"github.com/garyships/kudzu/internal/sparse"
	"github.com/garyships/kudzu/internal/store"
	"github.com/garyships/kudzu/internal/vfs"
)

// cmdMount presents a revision as a filesystem whose files are fetched the
// first time they are read: read-only, or with --writable an overlay that
// kudzu status, diff, commit, update and land use from inside the mount.
func cmdMount(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mount", flag.ContinueOnError)
	rev := fs.String("r", "main", "revision to mount (with --writable: where a new overlay starts; an existing one resumes unless -r is given)")
	sparseDirs := fs.String("sparse", "", "comma-separated directories to show (default: everything)")
	cacheDir := fs.String("cache", "", "keep fetched objects in this directory (default: in memory; a node daemon already caches them on disk)")
	maxCache := fs.String("max-cache", "2GB", "with --cache: disk budget")
	writable := fs.Bool("writable", false, "allow changes: they go to an overlay, and kudzu status/diff/commit/update/land work inside the mount")
	overlay := fs.String("overlay", "", "with --writable: overlay directory (default: one per mountpoint in your user cache dir); mounting it again resumes its changes")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return errors.New("usage: kudzu mount [--writable] [-r REV] [--sparse a,b] REMOTE MOUNTPOINT")
	}
	revSet := false
	fs.Visit(func(f *flag.Flag) { revSet = revSet || f.Name == "r" })
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
	if *writable {
		start := ""
		if revSet {
			start = *rev
		}
		return mountWritable(ctx, rem, pos[0], pos[1], *overlay, start, sp, cache)
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

func mountWritable(ctx context.Context, rem *proto.Remote, remote, mountpoint, overlay, rev string, sp sparse.Spec, cache store.Objects) error {
	if overlay == "" {
		abs, err := filepath.Abs(mountpoint)
		if err != nil {
			return err
		}
		base, err := os.UserCacheDir()
		if err != nil {
			base = os.TempDir()
		}
		sum := sha256.Sum256([]byte(abs))
		overlay = filepath.Join(base, "kudzu", "mounts", filepath.Base(abs)+"-"+hex.EncodeToString(sum[:4]))
	}
	o, err := vfs.OpenOverlay(ctx, overlay, rem, rev, sp, cache)
	if err != nil {
		return err
	}
	ctl, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		if err := o.ServeControl(ctl, nil); err != nil {
			fmt.Fprintf(os.Stderr, "kudzu mount: control socket: %v\n", err)
		}
	}()
	info := vfs.MountInfo{Control: vfs.ControlSocket(overlay), Remote: remote}
	srv, err := fusefs.MountWritable(mountpoint, o, info, log.New(os.Stderr, "kudzu-mount ", log.LstdFlags))
	if err != nil {
		return fmt.Errorf("mount %s: %w", mountpoint, err)
	}
	fmt.Printf("mounted %s writable at %s (overlay in %s)\nkudzu status, diff, commit and update work inside it; Ctrl-C to unmount\n", o.Base().Changeset.Short(), mountpoint, overlay)
	go func() {
		<-ctx.Done()
		srv.Unmount()
	}()
	srv.Wait()
	changes, err := o.Changes(context.Background())
	if err == nil && len(changes) > 0 {
		fmt.Printf("\nunmounted with %s uncommitted; mount the same overlay again to resume\n", plural(len(changes), "change", "changes"))
	} else {
		fmt.Println("\nunmounted")
	}
	return nil
}
