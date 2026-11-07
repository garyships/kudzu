package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/garyships/kudzu/internal/gitimport"
	"github.com/garyships/kudzu/internal/proto"
	"github.com/garyships/kudzu/internal/server"
	"github.com/garyships/kudzu/internal/store"
	"github.com/garyships/kudzu/internal/tier"
)

func defaultSocket() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(os.TempDir(), "kudzu.sock")
	}
	return "/tmp/kudzu.sock"
}

// upDataDir keeps one store per git repository, so re-running `kudzu up`
// only imports new commits.
func upDataDir(gitDir string) (string, error) {
	base, err := userCacheDir("--dir")
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(gitDir))
	return filepath.Join(base, "up", filepath.Base(gitDir)+"-"+hex.EncodeToString(sum[:4])), nil
}

// cmdUp is the one-command playground: import a git repository and run the
// server, a regional cache and a node daemon in one process.
func cmdUp(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	gitDir := fs.String("git", ".", "git repository to import (re-imports only new commits)")
	ref := fs.String("ref", "HEAD", "git revision to import")
	as := fs.String("as", "main", "kudzu ref to point at the imported tip")
	dir := fs.String("dir", "", "data directory (default: a per-repository folder in your user cache dir)")
	port := fs.Int("port", 0, "ignored: the server and cache listen on private unix sockets (kept so old scripts still run)")
	sock := fs.String("socket", defaultSocket(), "unix socket for the node daemon")
	sockMode := addSocketModeFlag(fs)
	force := fs.Bool("force", false, "move --as to the imported tip even if that drops changesets landed in kudzu")
	verbose := fs.Bool("v", false, "log every tier's requests to stderr")
	if _, err := parse(fs, args); err != nil {
		return err
	}

	absGit, err := filepath.Abs(*gitDir)
	if err != nil {
		return err
	}
	if out, err := exec.Command("git", "-C", absGit, "rev-parse", "--show-toplevel").Output(); err == nil {
		absGit = strings.TrimSpace(string(out))
	} else {
		return fmt.Errorf("%s is not a git repository (run inside one, or pass --git PATH)", absGit)
	}
	if *dir == "" {
		if *dir, err = upDataDir(absGit); err != nil {
			return err
		}
	}
	// Refuse to steal the socket from a live `kudzu up` or daemon.
	if c, err := net.Dial("unix", *sock); err == nil {
		c.Close()
		return fmt.Errorf("%s is already in use by a running kudzu; stop it or pass --socket", *sock)
	}

	r, closeRepo, err := openRepo(filepath.Join(*dir, "store"), 512<<20)
	if err != nil {
		return err
	}
	defer closeRepo()
	fmt.Printf("importing %s from %s\n", *ref, absGit)
	res, err := gitimport.Run(ctx, r, absGit, *ref, *as, *force, os.Stdout)
	if err != nil {
		return err
	}

	logOut := io.Discard
	if *verbose {
		logOut = os.Stderr
	}
	if *port != 0 {
		fmt.Fprintln(os.Stderr, "kudzu up: --port is ignored; the server and cache listen on unix sockets only you can reach")
	}
	// The server and cache serve the whole API without --auth, so they
	// listen where only this user can connect, like the daemon: on TCP,
	// even loopback, any local user could read the repository and land.
	tierDir, err := os.MkdirTemp("", "kudzu-up-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tierDir)
	serverAddr := "unix://" + filepath.Join(tierDir, "server.sock")
	cacheAddr := "unix://" + filepath.Join(tierDir, "cache.sock")
	serverL, err := listen(serverAddr, 0o600)
	if err != nil {
		return fmt.Errorf("server: %w", err)
	}
	cacheL, err := listen(cacheAddr, 0o600)
	if err != nil {
		serverL.Close()
		return fmt.Errorf("cache: %w", err)
	}
	daemonL, err := listen("unix://"+*sock, *sockMode)
	if err != nil {
		serverL.Close()
		cacheL.Close()
		return fmt.Errorf("daemon: %w", err)
	}
	defer os.Remove(*sock)

	srv := server.New(r, log.New(logOut, "kudzu-server ", log.LstdFlags))
	toServer, _ := proto.Dial(serverAddr)
	cache := tier.New("cache", store.NewMem(1<<30), toServer, 32)
	daemonObjs, err := store.OpenFS(filepath.Join(*dir, "daemon"), 10<<30)
	if err != nil {
		return err
	}
	toCache, _ := proto.Dial(cacheAddr)
	daemon := tier.New("daemon", daemonObjs, toCache, 8)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	run := func(fn func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fn(); err != nil {
				errs <- err
				cancel()
			}
		}()
	}
	run(func() error { return serveListener(ctx, serverL, srv.Handler()) })
	run(func() error { return serveListener(ctx, cacheL, cache.Handler()) })
	run(func() error { return serveListener(ctx, daemonL, daemon.Handler()) })

	remote := "unix://" + *sock
	fmt.Printf(`
kudzu is up: %d commits imported (%d new), %s -> %s

  server   %s
  cache    %s   (regional, in memory)
  daemon   %s   (this machine, on disk)
  data     %s

Try it (in another terminal):
  export KUDZU_REMOTE=%s
  kudzu log -n 5 --oneline
  kudzu blame <file>
  kudzu clone --sparse <dir> $KUDZU_REMOTE /tmp/wc
  claude mcp add kudzu -e KUDZU_REMOTE=%s -- kudzu mcp

Press Ctrl-C to stop.
`, res.Commits, res.Imported, *as, res.Ref.Short(), serverAddr, cacheAddr, remote, *dir, remote, remote)
	warnKept(os.Stderr, "up", res, *as, *ref, absGit, "--store "+filepath.Join(*dir, "store"))

	<-ctx.Done()
	wg.Wait()
	select {
	case err := <-errs:
		return err
	default:
	}
	fmt.Println("\nstopped")
	if errors.Is(ctx.Err(), context.Canceled) {
		return nil
	}
	return ctx.Err()
}
