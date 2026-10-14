package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/garyships/kudzu/internal/auth"
	"github.com/garyships/kudzu/internal/gitexport"
	"github.com/garyships/kudzu/internal/gitimport"
	"github.com/garyships/kudzu/internal/proto"
	"github.com/garyships/kudzu/internal/repo"
	"github.com/garyships/kudzu/internal/server"
	"github.com/garyships/kudzu/internal/store"
	"github.com/garyships/kudzu/internal/tier"
)

func openRepo(dir string, memBytes int64) (*repo.Repo, func(), error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, err
	}
	meta, err := store.OpenSQLite(filepath.Join(dir, "meta.db"))
	if err != nil {
		return nil, nil, err
	}
	objs, err := store.OpenFS(filepath.Join(dir, "objects"), 0)
	if err != nil {
		meta.Close()
		return nil, nil, err
	}
	r := repo.New(meta, &store.Cached{Mem: store.NewMem(memBytes), Backing: objs})
	return r, func() { meta.Close() }, nil
}

func listen(addr string) (net.Listener, error) {
	if sock, ok := strings.CutPrefix(addr, "unix://"); ok {
		os.Remove(sock)
		l, err := net.Listen("unix", sock)
		if err == nil {
			os.Chmod(sock, 0o666)
		}
		return l, err
	}
	return net.Listen("tcp", addr)
}

func serve(ctx context.Context, addr string, h http.Handler, logger *log.Logger) error {
	l, err := listen(addr)
	if err != nil {
		return err
	}
	logger.Printf("listening on %s", addr)
	return serveListener(ctx, l, h)
}

func serveListener(ctx context.Context, l net.Listener, h http.Handler) error {
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 30 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func cmdServer(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	dir := fs.String("store", "kudzu-data", "store directory (meta.db + objects/)")
	addr := fs.String("listen", "127.0.0.1:7070", "listen address (host:port or unix:///path)")
	mem := fs.String("mem", "512MB", "in-memory object cache size")
	lazy := fs.String("lazy", "", "comma-separated derived types to compute lazily (gen, unode)")
	authFile := fs.String("auth", "", "access policy file: require bearer tokens and enforce per-directory rules (see 'kudzu token')")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	var policy *auth.Policy
	if *authFile != "" {
		var err error
		if policy, err = auth.Load(*authFile); err != nil {
			return err
		}
	}
	memBytes, err := parseSize(*mem)
	if err != nil {
		return err
	}
	r, closeRepo, err := openRepo(*dir, memBytes)
	if err != nil {
		return err
	}
	defer closeRepo()
	if *lazy != "" {
		skip := map[string]bool{}
		for _, n := range strings.Split(*lazy, ",") {
			skip[strings.TrimSpace(n)] = true
		}
		var eager []*repo.Deriver
		for _, d := range r.Eager {
			if !skip[d.Name] {
				eager = append(eager, d)
			}
		}
		r.Eager = eager
	}
	logger := log.New(os.Stderr, "kudzu-server ", log.LstdFlags)
	srv := server.New(r, logger)
	srv.Auth = policy
	if policy != nil {
		logger.Printf("enforcing access policy %s", *authFile)
	}
	return serve(ctx, *addr, srv.Handler(), logger)
}

// cmdToken prints a new random token and the line to add to a policy file.
func cmdToken(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("token", flag.ContinueOnError)
	addJSONFlag(fs)
	if _, err := parse(fs, args); err != nil {
		return err
	}
	token, hash, err := auth.NewToken()
	if err != nil {
		return err
	}
	if jsonOutput {
		return printJSON(map[string]string{"token": token, "token_sha256": hash})
	}
	fmt.Printf("token:         %s\ntoken_sha256:  %s\n\nGive the token to the client (export KUDZU_TOKEN=...) and put the hash in\nthe principal's \"token_sha256\" list in the server's --auth policy file.\n", token, hash)
	return nil
}

func runTier(ctx context.Context, name, addr, upstream string, local store.Objects, concurrency int, seed string) error {
	up, err := proto.Dial(upstream)
	if err != nil {
		return err
	}
	t := tier.New(name, local, up, concurrency)
	logger := log.New(os.Stderr, "kudzu-"+name+" ", log.LstdFlags)
	if seed != "" {
		f, err := os.Open(seed)
		if err != nil {
			return err
		}
		start := time.Now()
		n, err := t.Seed(f)
		f.Close()
		if err != nil {
			return fmt.Errorf("seed: %w", err)
		}
		logger.Printf("seeded %d objects from %s in %s", n, seed, time.Since(start).Round(time.Millisecond))
	}
	logger.Printf("upstream %s", upstream)
	return serve(ctx, addr, t.Handler(), logger)
}

func cmdCache(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("cache", flag.ContinueOnError)
	addr := fs.String("listen", "127.0.0.1:7071", "listen address")
	upstream := fs.String("upstream", "127.0.0.1:7070", "upstream tier (normally the server)")
	mem := fs.String("mem", "1GB", "memory budget for cached objects")
	conc := fs.Int("upstream-concurrency", 32, "max in-flight upstream requests")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	memBytes, err := parseSize(*mem)
	if err != nil {
		return err
	}
	return runTier(ctx, "cache", *addr, *upstream, store.NewMem(memBytes), *conc, "")
}

func cmdDaemon(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	addr := fs.String("listen", "unix:///tmp/kudzu.sock", "listen address (unix:///path or host:port)")
	upstream := fs.String("upstream", "127.0.0.1:7071", "upstream tier (normally the regional cache)")
	dir := fs.String("dir", filepath.Join(os.TempDir(), "kudzu-daemon"), "object directory shared by all jobs on this node")
	maxDisk := fs.String("max-disk", "10GB", "disk budget for cached objects")
	conc := fs.Int("upstream-concurrency", 8, "max in-flight upstream requests")
	seed := fs.String("seed", "", "snapshot file to load at startup (see 'kudzu snapshot')")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	maxBytes, err := parseSize(*maxDisk)
	if err != nil {
		return err
	}
	objs, err := store.OpenFS(*dir, maxBytes)
	if err != nil {
		return err
	}
	return runTier(ctx, "daemon", *addr, *upstream, objs, *conc, *seed)
}

func cmdImport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	dir := fs.String("store", "kudzu-data", "server store directory")
	gitDir := fs.String("git", ".", "git repository to import from")
	ref := fs.String("ref", "HEAD", "git revision to import")
	as := fs.String("as", "main", "kudzu ref to point at the imported tip")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	r, closeRepo, err := openRepo(*dir, 256<<20)
	if err != nil {
		return err
	}
	defer closeRepo()
	fmt.Printf("importing %s from %s\n", *ref, *gitDir)
	res, err := gitimport.Run(ctx, r, *gitDir, *ref, *as, os.Stdout)
	if err != nil {
		return err
	}
	fmt.Printf("imported %d new of %d commits in %s; %s -> %s\n", res.Imported, res.Commits, res.Elapsed, *as, res.Tip.Short())
	return nil
}

func cmdExport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	dir := fs.String("store", "kudzu-data", "server store directory")
	gitDir := fs.String("git", "", "git repository to write to (bare, or with the branch not checked out)")
	ref := fs.String("ref", "main", "kudzu revision to export")
	as := fs.String("as", "", "git branch to point at the exported tip (default: same name as --ref)")
	force := fs.Bool("force", false, "move the branch even if it has commits kudzu lacks")
	addJSONFlag(fs)
	if _, err := parse(fs, args); err != nil {
		return err
	}
	if *gitDir == "" {
		return errors.New("--git is required")
	}
	if _, err := os.Stat(filepath.Join(*dir, "meta.db")); err != nil {
		return fmt.Errorf("%s is not a kudzu store: %w", *dir, err)
	}
	r, closeRepo, err := openRepo(*dir, 256<<20)
	if err != nil {
		return err
	}
	defer closeRepo()
	res, err := gitexport.Run(ctx, r, gitexport.Options{GitDir: *gitDir, Ref: *ref, Branch: *as, Force: *force})
	if err != nil {
		return err
	}
	if jsonOutput {
		return printJSON(res)
	}
	fmt.Printf("exported %s in %s; %s -> %s\n", plural(res.Exported, "new commit", "new commits"), res.Elapsed, res.Branch, res.Tip)
	return nil
}

func cmdSnapshot(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("snapshot", flag.ContinueOnError)
	remote := fs.String("remote", "", "server or cache address")
	rev := fs.String("r", "main", "revision")
	sparseDirs := fs.String("sparse", "", "comma-separated directories (default: everything)")
	out := fs.String("o", "kudzu-seed.frames", "output file")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	rem, err := dialRemote(*remote)
	if err != nil {
		return err
	}
	q := map[string][]string{"rev": {*rev}}
	if *sparseDirs != "" {
		q["sparse"] = []string{*sparseDirs}
	}
	body, err := rem.Stream(ctx, "/api/snapshot", q)
	if err != nil {
		return err
	}
	defer body.Close()
	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	fmt.Printf("wrote %s (%.1f MB)\n", *out, float64(n)/(1<<20))
	return nil
}

func cmdStats(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	remote := fs.String("remote", "", "tier address")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	rem, err := dialRemote(*remote)
	if err != nil {
		return err
	}
	var v map[string]any
	if err := rem.GetJSON(ctx, "/stats", nil, &v); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
	return nil
}
