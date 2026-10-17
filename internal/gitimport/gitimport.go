// Package gitimport ingests git history into Kudzu: every commit becomes a
// changeset listing the files changed relative to its first parent. Imports
// are incremental: commits already ingested are skipped, so re-running the
// import keeps a Kudzu repository in sync with a git mirror.
package gitimport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/garyships/kudzu/internal/lru"
	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/repo"
	"github.com/garyships/kudzu/internal/store"
)

const mappingNS = "git"

type Result struct {
	Commits  int       `json:"commits"`
	Imported int       `json:"imported"`
	Tip      object.ID `json:"tip"` // the imported git revision's changeset
	// Ref is where the Kudzu ref points after the import, and RefMoved says
	// whether the import moved it there.
	Ref      object.ID `json:"ref"`
	RefMoved bool      `json:"ref_moved"`
	// Kept means the ref was left alone because it has changesets that Tip
	// lacks, typically work landed through Kudzu since the last import.
	// Diverged means Tip also has commits the ref lacks; otherwise Tip is
	// already part of the ref's history.
	Kept     bool   `json:"kept,omitempty"`
	Diverged bool   `json:"diverged,omitempty"`
	Elapsed  string `json:"elapsed"`
}

type importer struct {
	repo    *repo.Repo
	gitDir  string
	cat     *catFile
	hashLen int
	blobs   *lru.Cache[string, object.ID] // git blob sha -> kudzu blob id
	trees   *lru.Cache[string, []gitEntry]
	commits map[string]object.ID // git commit sha -> changeset (this run)
	treeOf  map[string]string    // git commit sha -> git tree sha (this run)
}

// Run imports ref from the git repository at gitDir and points kudzuRef at
// the result if that is a fast-forward: kudzuRef does not exist yet or is
// part of the imported history. Otherwise kudzuRef keeps the changesets
// landed on it and is left alone (Result.Kept), unless force.
func Run(ctx context.Context, r *repo.Repo, gitDir, ref, kudzuRef string, force bool, progress io.Writer) (*Result, error) {
	start := time.Now()
	if err := repo.ValidateRefName(kudzuRef); err != nil {
		return nil, err
	}
	im := &importer{
		repo:    r,
		gitDir:  gitDir,
		hashLen: 20,
		blobs:   lru.New[string, object.ID](500_000),
		trees:   lru.New[string, []gitEntry](50_000),
		commits: map[string]object.ID{},
		treeOf:  map[string]string{},
	}
	if out, err := im.git("rev-parse", "--show-object-format"); err == nil && strings.TrimSpace(out) == "sha256" {
		im.hashLen = 32
	}
	list, err := im.git("rev-list", "--topo-order", "--reverse", "--parents", ref)
	if err != nil {
		return nil, err
	}
	cat, err := startCatFile(gitDir)
	if err != nil {
		return nil, err
	}
	defer cat.close()
	im.cat = cat

	lines := strings.Split(strings.TrimSpace(list), "\n")
	res := &Result{Commits: len(lines)}
	var tip object.ID
	for i, line := range lines {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		sha, parents := fields[0], fields[1:]
		id, created, err := im.importCommit(ctx, sha, parents)
		if err != nil {
			return nil, fmt.Errorf("commit %s: %w", sha, err)
		}
		if created {
			res.Imported++
		}
		tip = id
		if progress != nil && (i+1)%200 == 0 {
			rate := float64(i+1) / time.Since(start).Seconds()
			fmt.Fprintf(progress, "  %d/%d commits (%.0f/s)\n", i+1, len(lines), rate)
		}
	}
	res.Tip = tip
	if !tip.IsZero() {
		if err := moveRef(ctx, r, kudzuRef, tip, force, res); err != nil {
			return nil, err
		}
	}
	res.Elapsed = time.Since(start).Round(time.Millisecond).String()
	return res, nil
}

// moveRef points name at tip, by compare-and-swap, unless that would drop
// changesets from it: re-importing must not rewind work landed in Kudzu.
func moveRef(ctx context.Context, r *repo.Repo, name string, tip object.ID, force bool, res *Result) error {
	for attempt := 0; ; attempt++ {
		old, err := r.Meta.GetRef(ctx, name)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		res.Ref = old
		if old == tip {
			return nil
		}
		if !old.IsZero() && !force {
			ff, err := r.IsAncestor(ctx, old, tip)
			if err != nil {
				return err
			}
			if !ff {
				behind, err := r.IsAncestor(ctx, tip, old)
				if err != nil {
					return err
				}
				res.Kept, res.Diverged = true, !behind
				return nil
			}
		}
		err = r.Meta.CASRef(ctx, name, old, tip)
		if err == nil {
			res.Ref, res.RefMoved = tip, true
			return nil
		}
		if !errors.Is(err, store.ErrConflict) || attempt > 10 {
			return err
		}
	}
}

func (im *importer) git(args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", im.gitDir}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

func (im *importer) mapped(ctx context.Context, sha string) (object.ID, bool, error) {
	if id, ok := im.commits[sha]; ok {
		return id, true, nil
	}
	v, err := im.repo.Meta.GetMapping(ctx, mappingNS, sha)
	if errors.Is(err, store.ErrNotFound) {
		return object.Zero, false, nil
	}
	if err != nil {
		return object.Zero, false, err
	}
	id, err := object.ParseID(v)
	if err == nil {
		im.commits[sha] = id
	}
	return id, err == nil, err
}

func (im *importer) importCommit(ctx context.Context, sha string, parentShas []string) (object.ID, bool, error) {
	if id, ok, err := im.mapped(ctx, sha); err != nil || ok {
		return id, false, err
	}
	typ, data, err := im.cat.get(sha)
	if err != nil {
		return object.Zero, false, err
	}
	if typ != "commit" {
		return object.Zero, false, fmt.Errorf("expected commit, got %s", typ)
	}
	gc, err := parseCommit(data)
	if err != nil {
		return object.Zero, false, err
	}
	im.treeOf[sha] = gc.tree

	var parents []object.ID
	for _, p := range parentShas {
		id, ok, err := im.mapped(ctx, p)
		if err != nil {
			return object.Zero, false, err
		}
		if ok {
			parents = append(parents, id)
		}
	}
	baseTree := ""
	if len(parentShas) > 0 {
		if _, ok := im.commits[parentShas[0]]; ok {
			if baseTree, err = im.commitTree(parentShas[0]); err != nil {
				return object.Zero, false, err
			}
		} else {
			// Shallow boundary: the parent is absent, so this commit starts history.
			parents = nil
		}
	}
	var pending []pendingChange
	if err := im.diff(ctx, baseTree, gc.tree, "", &pending); err != nil {
		return object.Zero, false, err
	}
	changes, err := im.resolveBlobs(ctx, pending)
	if err != nil {
		return object.Zero, false, err
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	created, err := im.repo.Create(ctx, repo.CreateRequest{
		Parents: parents,
		Author:  gc.author,
		Time:    gc.time,
		TZ:      gc.tz,
		Message: gc.message,
		Changes: changes,
		Extra:   map[string]string{"git": sha},
	})
	if err != nil {
		return object.Zero, false, err
	}
	if err := im.repo.Meta.PutMapping(ctx, mappingNS, sha, created.ID.String()); err != nil {
		return object.Zero, false, err
	}
	im.commits[sha] = created.ID
	return created.ID, true, nil
}

func (im *importer) commitTree(sha string) (string, error) {
	if t, ok := im.treeOf[sha]; ok {
		return t, nil
	}
	_, data, err := im.cat.get(sha)
	if err != nil {
		return "", err
	}
	gc, err := parseCommit(data)
	if err != nil {
		return "", err
	}
	im.treeOf[sha] = gc.tree
	return gc.tree, nil
}

type gitEntry struct {
	name string
	mode uint32
	sha  string
}

func (e gitEntry) isDir() bool { return e.mode == object.ModeDir }

func normalizeMode(m uint32) uint32 {
	switch {
	case m == 0o40000:
		return object.ModeDir
	case m == object.ModeSymlink, m == object.ModeGitlink:
		return m
	case m&0o111 != 0:
		return object.ModeExec
	}
	return object.ModeFile
}

func (im *importer) readTree(sha string) ([]gitEntry, error) {
	if sha == "" {
		return nil, nil
	}
	if es, ok := im.trees.Get(sha); ok {
		return es, nil
	}
	typ, data, err := im.cat.get(sha)
	if err != nil {
		return nil, err
	}
	if typ != "tree" {
		return nil, fmt.Errorf("%s is a %s, not a tree", sha, typ)
	}
	var es []gitEntry
	for len(data) > 0 {
		sp := bytes.IndexByte(data, ' ')
		nul := bytes.IndexByte(data, 0)
		if sp < 0 || nul < sp || len(data) < nul+1+im.hashLen {
			return nil, fmt.Errorf("malformed tree %s", sha)
		}
		mode, err := strconv.ParseUint(string(data[:sp]), 8, 32)
		if err != nil {
			return nil, err
		}
		es = append(es, gitEntry{
			name: string(data[sp+1 : nul]),
			mode: normalizeMode(uint32(mode)),
			sha:  hex.EncodeToString(data[nul+1 : nul+1+im.hashLen]),
		})
		data = data[nul+1+im.hashLen:]
	}
	im.trees.Add(sha, es)
	return es, nil
}

// diff compares two git trees, skipping identical subtrees by hash.
func (im *importer) diff(ctx context.Context, a, b, prefix string, out *[]pendingChange) error {
	if a == b {
		return nil
	}
	ea, err := im.readTree(a)
	if err != nil {
		return err
	}
	eb, err := im.readTree(b)
	if err != nil {
		return err
	}
	old := make(map[string]gitEntry, len(ea))
	for _, e := range ea {
		old[e.name] = e
	}
	for _, n := range eb {
		p := object.JoinPath(prefix, n.name)
		o, had := old[n.name]
		delete(old, n.name)
		switch {
		case had && o.mode == n.mode && o.sha == n.sha:
		case had && o.isDir() && n.isDir():
			if err := im.diff(ctx, o.sha, n.sha, p, out); err != nil {
				return err
			}
		default:
			// A file replaced by a directory (or vice versa) needs the old
			// side removed; a file replaced by a file is just a modification.
			if had && o.isDir() != n.isDir() {
				if err := im.remove(o, p, out); err != nil {
					return err
				}
			}
			if err := im.add(ctx, n, p, out); err != nil {
				return err
			}
		}
	}
	for _, o := range old {
		if err := im.remove(o, object.JoinPath(prefix, o.name), out); err != nil {
			return err
		}
	}
	return nil
}

func (im *importer) remove(e gitEntry, p string, out *[]pendingChange) error {
	if !e.isDir() {
		*out = append(*out, pendingChange{Change: object.Change{Path: p}})
		return nil
	}
	es, err := im.readTree(e.sha)
	if err != nil {
		return err
	}
	for _, c := range es {
		if err := im.remove(c, object.JoinPath(p, c.name), out); err != nil {
			return err
		}
	}
	return nil
}

func (im *importer) add(ctx context.Context, e gitEntry, p string, out *[]pendingChange) error {
	if e.isDir() {
		es, err := im.readTree(e.sha)
		if err != nil {
			return err
		}
		for _, c := range es {
			if err := im.add(ctx, c, object.JoinPath(p, c.name), out); err != nil {
				return err
			}
		}
		return nil
	}
	*out = append(*out, pendingChange{Change: object.Change{Path: p, Mode: e.mode}, sha: e.sha})
	return nil
}

// pendingChange is a change whose blob is still identified by its git sha.
type pendingChange struct {
	object.Change
	sha string
}

// resolveBlobs reads new blobs from git (one cat-file process, so in
// sequence) and hashes, compresses and stores them on a worker pool.
func (im *importer) resolveBlobs(ctx context.Context, pending []pendingChange) ([]object.Change, error) {
	type job struct {
		sha     string
		content []byte
	}
	jobs := make(chan job, 64)
	var mu sync.Mutex
	results := map[string]object.ID{}
	var firstErr error
	var wg sync.WaitGroup
	for i := 0; i < runtime.GOMAXPROCS(0); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				id, err := im.repo.WriteBlob(ctx, j.content)
				mu.Lock()
				if err != nil && firstErr == nil {
					firstErr = err
				}
				results[j.sha] = id
				mu.Unlock()
			}
		}()
	}
	queued := map[string]bool{}
	var readErr error
	for _, pc := range pending {
		if pc.Deleted() || queued[pc.sha] {
			continue
		}
		if _, ok := im.blobs.Get(pc.sha); ok {
			continue
		}
		queued[pc.sha] = true
		if pc.Mode == object.ModeGitlink {
			// Submodules are recorded as a blob holding the pinned commit.
			jobs <- job{pc.sha, []byte(pc.sha)}
			continue
		}
		typ, data, err := im.cat.get(pc.sha)
		if err == nil && typ != "blob" {
			err = fmt.Errorf("%s is a %s, not a blob", pc.sha, typ)
		}
		if err != nil {
			readErr = err
			break
		}
		jobs <- job{pc.sha, data}
	}
	close(jobs)
	wg.Wait()
	if readErr != nil {
		return nil, readErr
	}
	if firstErr != nil {
		return nil, firstErr
	}
	for sha, id := range results {
		im.blobs.Add(sha, id)
	}
	changes := make([]object.Change, len(pending))
	for i, pc := range pending {
		changes[i] = pc.Change
		if pc.Deleted() {
			continue
		}
		id, ok := results[pc.sha]
		if !ok {
			id, _ = im.blobs.Get(pc.sha)
		}
		changes[i].Blob = id
	}
	return changes, nil
}

type gitCommit struct {
	tree    string
	author  string
	time    int64
	tz      int
	message string
}

func parseCommit(data []byte) (*gitCommit, error) {
	header, msg, _ := bytes.Cut(data, []byte("\n\n"))
	gc := &gitCommit{message: strings.TrimRight(string(msg), "\n")}
	for _, line := range strings.Split(string(header), "\n") {
		key, val, _ := strings.Cut(line, " ")
		switch key {
		case "tree":
			gc.tree = val
		case "author":
			// "Name <email> 1700000000 +0800"
			f := strings.Fields(val)
			if len(f) < 2 {
				return nil, fmt.Errorf("bad author line %q", val)
			}
			gc.author = strings.Join(f[:len(f)-2], " ")
			gc.time, _ = strconv.ParseInt(f[len(f)-2], 10, 64)
			if tz := f[len(f)-1]; len(tz) == 5 {
				h, _ := strconv.Atoi(tz[1:3])
				m, _ := strconv.Atoi(tz[3:5])
				gc.tz = h*60 + m
				if tz[0] == '-' {
					gc.tz = -gc.tz
				}
			}
		}
	}
	if gc.tree == "" {
		return nil, errors.New("commit has no tree")
	}
	return gc, nil
}

// catFile is one long-running `git cat-file --batch` process.
type catFile struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	w   *bufio.Writer
	out *bufio.Reader
}

func startCatFile(gitDir string) (*catFile, error) {
	cmd := exec.Command("git", "-C", gitDir, "cat-file", "--batch")
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &catFile{cmd: cmd, in: in, w: bufio.NewWriter(in), out: bufio.NewReaderSize(out, 1<<20)}, nil
}

func (c *catFile) get(sha string) (string, []byte, error) {
	if _, err := c.w.WriteString(sha + "\n"); err != nil {
		return "", nil, err
	}
	if err := c.w.Flush(); err != nil {
		return "", nil, err
	}
	header, err := c.out.ReadString('\n')
	if err != nil {
		return "", nil, err
	}
	f := strings.Fields(header)
	if len(f) == 2 && f[1] == "missing" {
		return "", nil, fmt.Errorf("git object %s is missing", sha)
	}
	if len(f) != 3 {
		return "", nil, fmt.Errorf("unexpected cat-file header %q", header)
	}
	size, err := strconv.Atoi(f[2])
	if err != nil {
		return "", nil, err
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(c.out, data); err != nil {
		return "", nil, err
	}
	if _, err := c.out.ReadByte(); err != nil {
		return "", nil, err
	}
	return f[1], data, nil
}

func (c *catFile) close() {
	c.in.Close()
	c.cmd.Wait()
}
