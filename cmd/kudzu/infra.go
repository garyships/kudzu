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
	"runtime"
	"strconv"
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

// storeFlags select a server's storage: a local directory by default, or
// shared backends that let several stateless servers serve one repository.
type storeFlags struct {
	dir, meta, objects *string
}

func addStoreFlags(fs *flag.FlagSet, defaultDir string) storeFlags {
	return storeFlags{
		dir:     fs.String("store", defaultDir, "store directory (meta.db + objects/)"),
		meta:    fs.String("meta", "", "shared metadata database instead of <store>/meta.db (postgres://user:pass@host/db)"),
		objects: fs.String("objects", "", "shared object store instead of <store>/objects (s3://bucket/prefix?endpoint=URL&region=R, gs://bucket/prefix; credentials from AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY)"),
	}
}

func (sf storeFlags) set() bool { return *sf.dir != "" || *sf.meta != "" || *sf.objects != "" }

// open opens the selected stores. With mustExist, a local store that has no
// meta.db yet is an error rather than a new, empty repository.
func (sf storeFlags) open(ctx context.Context, memBytes int64, mustExist bool) (*repo.Repo, func(), error) {
	if *sf.meta == "" && *sf.objects == "" {
		if mustExist {
			if _, err := os.Stat(filepath.Join(*sf.dir, "meta.db")); err != nil {
				return nil, nil, fmt.Errorf("%s is not a kudzu store: %w", *sf.dir, err)
			}
		}
		return openRepo(*sf.dir, memBytes)
	}
	if *sf.dir == "" && (*sf.meta == "" || *sf.objects == "") {
		return nil, nil, errors.New("pass --store for whichever of --meta and --objects is not given")
	}
	var meta store.Meta
	if u := *sf.meta; u != "" {
		if !strings.HasPrefix(u, "postgres://") && !strings.HasPrefix(u, "postgresql://") {
			return nil, nil, fmt.Errorf("unsupported --meta %q (use postgres://...)", u)
		}
		pg, err := store.OpenPostgres(ctx, u)
		if err != nil {
			return nil, nil, err
		}
		meta = pg
	} else {
		if err := os.MkdirAll(*sf.dir, 0o755); err != nil {
			return nil, nil, err
		}
		sq, err := store.OpenSQLite(filepath.Join(*sf.dir, "meta.db"))
		if err != nil {
			return nil, nil, err
		}
		meta = sq
	}
	var objs store.Objects
	if u := *sf.objects; u != "" {
		cfg, err := store.ParseObjectsURL(u)
		if err == nil {
			objs, err = store.OpenS3(ctx, cfg)
		}
		if err != nil {
			meta.Close()
			return nil, nil, err
		}
	} else {
		fs, err := store.OpenDurableFS(filepath.Join(*sf.dir, "objects"))
		if err != nil {
			meta.Close()
			return nil, nil, err
		}
		objs = fs
	}
	r := repo.New(meta, &store.Cached{Mem: store.NewMem(memBytes), Backing: objs})
	return r, func() { meta.Close() }, nil
}

func openRepo(dir string, memBytes int64) (*repo.Repo, func(), error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, err
	}
	meta, err := store.OpenSQLite(filepath.Join(dir, "meta.db"))
	if err != nil {
		return nil, nil, err
	}
	objs, err := store.OpenDurableFS(filepath.Join(dir, "objects"))
	if err != nil {
		meta.Close()
		return nil, nil, err
	}
	r := repo.New(meta, &store.Cached{Mem: store.NewMem(memBytes), Backing: objs})
	return r, func() { meta.Close() }, nil
}

// socketMode is the --socket-mode flag: who may connect to a unix socket a
// tier listens on. Whoever can connect can use the whole API through it,
// landing changes and moving refs included, so the default is the owner only.
type socketMode os.FileMode

func addSocketModeFlag(fs *flag.FlagSet) *socketMode {
	m := socketMode(0o600)
	fs.Var(&m, "socket-mode", "permissions of a unix:// listen socket, in octal: 0600 lets only you connect; 0660 or 0666 share the tier with your group or every local user, who can then land changes and move refs through it")
	return &m
}

func (m *socketMode) String() string { return fmt.Sprintf("%#o", uint32(*m)) }

func (m *socketMode) Set(s string) error {
	v, err := strconv.ParseUint(s, 8, 32)
	if err != nil || v > 0o777 {
		return errors.New("want octal permissions such as 0600, 0660 or 0666")
	}
	*m = socketMode(v)
	return nil
}

func listen(addr string, mode socketMode) (net.Listener, error) {
	if sock, ok := strings.CutPrefix(addr, "unix://"); ok {
		// A stale socket we cannot remove belongs to someone else.
		if err := os.Remove(sock); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		l, err := listenUnix(sock, os.FileMode(mode))
		if err != nil {
			return nil, err
		}
		if err := os.Chmod(sock, os.FileMode(mode)); err != nil && runtime.GOOS != "windows" {
			l.Close()
			return nil, err
		}
		return l, nil
	}
	return net.Listen("tcp", addr)
}

func serve(ctx context.Context, addr string, mode socketMode, h http.Handler, logger *log.Logger) error {
	l, err := listen(addr, mode)
	if err != nil {
		return err
	}
	logger.Printf("listening on %s", addr)
	return serveListener(ctx, l, h)
}

// idleTimeout closes keep-alive connections nobody uses. Clients drop theirs
// after 90 seconds (proto.Dial), so normally they close first.
var idleTimeout = 2 * time.Minute

func serveListener(ctx context.Context, l net.Listener, h http.Handler) error {
	// No ReadTimeout or WriteTimeout: uploads, snapshots and lands can
	// rightly take longer than any fixed bound.
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 30 * time.Second, IdleTimeout: idleTimeout}
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
	stores := addStoreFlags(fs, "kudzu-data")
	addr := fs.String("listen", "127.0.0.1:7070", "listen address (host:port or unix:///path)")
	sockMode := addSocketModeFlag(fs)
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
	r, closeRepo, err := stores.open(ctx, memBytes, false)
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
	return serve(ctx, *addr, *sockMode, srv.Handler(), logger)
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

func runTier(ctx context.Context, name, addr string, mode socketMode, upstream string, local store.Objects, concurrency int, seed string) error {
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
	return serve(ctx, addr, mode, t.Handler(), logger)
}

func cmdCache(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("cache", flag.ContinueOnError)
	addr := fs.String("listen", "127.0.0.1:7071", "listen address")
	sockMode := addSocketModeFlag(fs)
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
	return runTier(ctx, "cache", *addr, *sockMode, *upstream, store.NewMem(memBytes), *conc, "")
}

// daemonDir is the node daemon's default object directory. It is per user,
// like the socket, rather than a fixed path in the shared temp directory.
func daemonDir() string {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "kudzu", "daemon")
}

func cmdDaemon(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	addr := fs.String("listen", "unix:///tmp/kudzu.sock", "listen address (unix:///path or host:port)")
	sockMode := addSocketModeFlag(fs)
	upstream := fs.String("upstream", "127.0.0.1:7071", "upstream tier (normally the regional cache)")
	dir := fs.String("dir", daemonDir(), "object directory shared by all jobs on this node (created owner-only)")
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
	if err := os.MkdirAll(*dir, 0o700); err != nil {
		return err
	}
	objs, err := store.OpenFS(*dir, maxBytes)
	if err != nil {
		return err
	}
	return runTier(ctx, "daemon", *addr, *sockMode, *upstream, objs, *conc, *seed)
}

func cmdImport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	stores := addStoreFlags(fs, "kudzu-data")
	gitDir := fs.String("git", ".", "git repository to import from")
	ref := fs.String("ref", "HEAD", "git revision to import")
	as := fs.String("as", "main", "kudzu ref to point at the imported tip")
	force := fs.Bool("force", false, "move --as to the imported tip even if that drops changesets landed in kudzu")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	r, closeRepo, err := stores.open(ctx, 256<<20, false)
	if err != nil {
		return err
	}
	defer closeRepo()
	fmt.Printf("importing %s from %s\n", *ref, *gitDir)
	res, err := gitimport.Run(ctx, r, *gitDir, *ref, *as, *force, os.Stdout)
	if err != nil {
		return err
	}
	fmt.Printf("imported %d new of %d commits in %s; %s -> %s\n", res.Imported, res.Commits, res.Elapsed, *as, res.Ref.Short())
	storeArgs := "--store " + *stores.dir
	if *stores.meta != "" || *stores.objects != "" {
		storeArgs = "<your --store/--meta/--objects flags>"
	}
	warnKept(os.Stderr, "import", res, *as, *ref, *gitDir, storeArgs)
	return nil
}

// warnKept explains an import that left the kudzu ref where it was because
// it has changesets the git revision lacks, and how to reconcile the two.
func warnKept(w io.Writer, cmd string, res *gitimport.Result, as, ref, gitDir, storeArgs string) {
	if !res.Kept {
		return
	}
	why := fmt.Sprintf("it has changesets that %s (imported as %s) lacks", ref, res.Tip.Short())
	if res.Diverged {
		why = fmt.Sprintf("it and %s (imported as %s) have diverged", ref, res.Tip.Short())
	}
	fmt.Fprintf(w, "\nkudzu %s: warning: left %s at %s: %s, and moving it would drop the changesets landed in kudzu.\n",
		cmd, as, res.Ref.Short(), why)
	fmt.Fprintf(w, `  To keep them, export them to a git branch, merge that into %s and import again:
    kudzu export %s --git %s --ref %s --as kudzu-%s
  To move %s to %s anyway (the changesets stay in the store), re-run with --force.
`, ref, storeArgs, gitDir, as, as, as, ref)
}

func cmdExport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	stores := addStoreFlags(fs, "kudzu-data")
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
	r, closeRepo, err := stores.open(ctx, 256<<20, true)
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
		// A daemon seeded from a partial snapshot would not know it.
		os.Remove(*out)
		return fmt.Errorf("snapshot of %s not written: %w", *rev, err)
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
