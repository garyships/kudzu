// Package gitexport writes Kudzu history into a git repository, the reverse
// of gitimport: every changeset not yet in git becomes a git commit, and a
// branch is moved to the exported tip. Exports are incremental, so re-running
// one keeps a git mirror of a Kudzu ref up to date for tools that need git
// (code hosts, CI that clones, code search).
//
// Changesets list their changes relative to their first parent, which is
// exactly how git fast-import describes a commit, so each changeset is
// streamed to one long-running fast-import process as-is.
package gitexport

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/repo"
	"github.com/garyships/kudzu/internal/store"
)

const (
	// importNS maps git sha -> changeset (shared with gitimport, so that a
	// later import from the mirror skips commits that came from Kudzu).
	importNS = "git"
	// exportNS maps changeset -> git sha for changesets created in Kudzu.
	exportNS = "git-export"
)

// ErrNotFastForward means the git branch has commits the export would drop.
var ErrNotFastForward = errors.New("not a fast-forward")

type Options struct {
	GitDir string // repository to write to (bare, or with the branch not checked out)
	Ref    string // Kudzu revision to export
	Branch string // git ref to update; a bare name means refs/heads/NAME
	Force  bool   // move the branch even if that drops git commits
}

type Result struct {
	Changesets int    `json:"changesets"` // reachable changesets not already in git
	Exported   int    `json:"exported"`   // commits written
	Tip        string `json:"tip"`        // git sha of the exported revision
	Branch     string `json:"branch"`
	Previous   string `json:"previous,omitempty"` // branch's old target
	Elapsed    string `json:"elapsed"`
}

// Run exports opts.Ref and everything below it that git lacks.
func Run(ctx context.Context, r *repo.Repo, opts Options) (*Result, error) {
	start := time.Now()
	branch := opts.Branch
	if branch == "" {
		branch = opts.Ref
	}
	if !strings.HasPrefix(branch, "refs/") {
		branch = "refs/heads/" + branch
	}
	if err := gitRun(ctx, opts.GitDir, nil, "check-ref-format", branch); err != nil {
		return nil, fmt.Errorf("bad branch name %q", branch)
	}
	tip, err := r.Resolve(ctx, opts.Ref)
	if err != nil {
		return nil, err
	}
	check, err := startBatchCheck(opts.GitDir)
	if err != nil {
		return nil, err
	}
	defer check.close()
	ex := &exporter{repo: r, check: check, known: map[object.ID]string{}}

	order, err := ex.plan(ctx, tip)
	if err != nil {
		return nil, err
	}
	res := &Result{Changesets: len(order), Branch: branch}
	if len(order) > 0 {
		if err := ex.write(ctx, opts.GitDir, order); err != nil {
			return nil, err
		}
		res.Exported = len(order)
	}
	res.Tip = ex.known[tip]

	old, err := gitOutput(ctx, opts.GitDir, "rev-parse", "-q", "--verify", branch+"^{commit}")
	old = strings.TrimSpace(old)
	if err != nil {
		old = "" // the branch does not exist yet
	}
	res.Previous = old
	if old != res.Tip {
		if old != "" && !opts.Force {
			if err := gitRun(ctx, opts.GitDir, nil, "merge-base", "--is-ancestor", old, res.Tip); err != nil {
				return nil, fmt.Errorf("%w: %s is at %s, which %s does not contain (use --force to overwrite)", ErrNotFastForward, branch, short(old), opts.Ref)
			}
		}
		// Compare-and-swap, like a Kudzu ref: fail if someone moved it since.
		args := []string{"update-ref", "-m", "kudzu export " + opts.Ref, branch, res.Tip}
		if old != "" {
			args = append(args, old)
		} else {
			args = append(args, "")
		}
		if err := gitRun(ctx, opts.GitDir, nil, args...); err != nil {
			return nil, err
		}
	}
	res.Elapsed = time.Since(start).Round(time.Millisecond).String()
	return res, nil
}

type exporter struct {
	repo  *repo.Repo
	check *batchCheck
	known map[object.ID]string // changeset -> git sha present in the target
}

// inGit returns the git commit a changeset already is in the target
// repository, if any: one exported earlier, or the commit it was imported
// from (only if that mapping still points back at it, since rebased copies
// keep their original's extra.git).
func (ex *exporter) inGit(ctx context.Context, id object.ID) (string, bool, error) {
	if sha, ok := ex.known[id]; ok {
		return sha, true, nil
	}
	var candidates []string
	if sha, err := ex.repo.Meta.GetMapping(ctx, exportNS, id.String()); err == nil {
		candidates = append(candidates, sha)
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", false, err
	}
	c, err := ex.repo.Changeset(ctx, id)
	if err != nil {
		return "", false, err
	}
	if sha := c.Extra["git"]; sha != "" {
		if v, err := ex.repo.Meta.GetMapping(ctx, importNS, sha); err == nil && v == id.String() {
			candidates = append(candidates, sha)
		} else if err != nil && !errors.Is(err, store.ErrNotFound) {
			return "", false, err
		}
	}
	for _, sha := range candidates {
		ok, err := ex.check.isCommit(sha)
		if err != nil {
			return "", false, err
		}
		if ok {
			ex.known[id] = sha
			return sha, true, nil
		}
	}
	return "", false, nil
}

// plan returns the changesets reachable from tip that git lacks, parents
// before children.
func (ex *exporter) plan(ctx context.Context, tip object.ID) ([]object.ID, error) {
	type frame struct {
		id       object.ID
		expanded bool
	}
	var order []object.ID
	visited := map[object.ID]bool{}
	stack := []frame{{id: tip}}
	for len(stack) > 0 {
		f := stack[len(stack)-1]
		if f.expanded {
			stack = stack[:len(stack)-1]
			order = append(order, f.id)
			continue
		}
		if visited[f.id] {
			stack = stack[:len(stack)-1]
			continue
		}
		visited[f.id] = true
		if _, ok, err := ex.inGit(ctx, f.id); err != nil {
			return nil, err
		} else if ok {
			stack = stack[:len(stack)-1]
			continue
		}
		stack[len(stack)-1].expanded = true
		c, err := ex.repo.Changeset(ctx, f.id)
		if err != nil {
			return nil, err
		}
		for i := len(c.Parents) - 1; i >= 0; i-- {
			if !visited[c.Parents[i]] {
				stack = append(stack, frame{id: c.Parents[i]})
			}
		}
	}
	return order, nil
}

// write streams order to git fast-import, then records the new commits.
func (ex *exporter) write(ctx context.Context, gitDir string, order []object.ID) error {
	tmp, err := os.MkdirTemp("", "kudzu-export-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	marksFile := filepath.Join(tmp, "marks")
	// A scratch branch: fast-import needs one, and the user's branch is only
	// moved afterwards, by compare-and-swap.
	scratch := fmt.Sprintf("refs/kudzu-export/%d-%d", os.Getpid(), time.Now().UnixNano())
	cmd := exec.CommandContext(ctx, "git", "-C", gitDir, "fast-import", "--quiet", "--done", "--export-marks="+marksFile)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	w := bufio.NewWriterSize(in, 1<<20)
	streamErr := ex.stream(ctx, w, scratch, order)
	if streamErr == nil {
		streamErr = w.Flush()
	}
	in.Close()
	waitErr := cmd.Wait()
	gitRun(context.Background(), gitDir, nil, "update-ref", "-d", scratch)
	if waitErr != nil {
		return fmt.Errorf("git fast-import: %v: %s", waitErr, strings.TrimSpace(stderr.String()))
	}
	if streamErr != nil {
		return streamErr
	}

	marks, err := readMarks(marksFile)
	if err != nil {
		return err
	}
	for i, id := range order {
		sha, ok := marks[i+1]
		if !ok {
			return fmt.Errorf("git fast-import did not report a commit for %s", id.Short())
		}
		ex.known[id] = sha
		if err := ex.repo.Meta.PutMapping(ctx, exportNS, id.String(), sha); err != nil {
			return err
		}
		if err := ex.repo.Meta.PutMapping(ctx, importNS, sha, id.String()); err != nil {
			return err
		}
	}
	return nil
}

// stream writes one fast-import commit per changeset. Changeset i gets mark
// :i+1; blobs get marks after those, and each distinct blob is sent once.
func (ex *exporter) stream(ctx context.Context, w *bufio.Writer, scratch string, order []object.ID) error {
	marks := make(map[object.ID]int, len(order))
	for i, id := range order {
		marks[id] = i + 1
	}
	blobMarks := map[object.ID]int{}
	nextMark := len(order) + 1
	ref := func(id object.ID) string {
		if m, ok := marks[id]; ok {
			return ":" + strconv.Itoa(m)
		}
		return ex.known[id]
	}
	for i, id := range order {
		if err := ctx.Err(); err != nil {
			return err
		}
		c, err := ex.repo.Changeset(ctx, id)
		if err != nil {
			return err
		}
		// Blobs first: fast-import wants them before the commit that uses them.
		var mods []string
		var dels []string
		for _, ch := range c.Changes {
			if ch.Deleted() {
				dels = append(dels, "D "+quotePath(ch.Path)+"\n")
				continue
			}
			content, err := ex.repo.ReadBlob(ctx, ch.Blob)
			if err != nil {
				return fmt.Errorf("%s: %w", ch.Path, err)
			}
			if ch.Mode == object.ModeGitlink {
				// gitimport stores a submodule as a blob holding the commit sha.
				mods = append(mods, fmt.Sprintf("M 160000 %s %s\n", strings.TrimSpace(string(content)), quotePath(ch.Path)))
				continue
			}
			m, ok := blobMarks[ch.Blob]
			if !ok {
				m = nextMark
				nextMark++
				blobMarks[ch.Blob] = m
				fmt.Fprintf(w, "blob\nmark :%d\ndata %d\n", m, len(content))
				w.Write(content)
				w.WriteString("\n")
			}
			mods = append(mods, fmt.Sprintf("M %o :%d %s\n", ch.Mode, m, quotePath(ch.Path)))
		}
		if len(c.Parents) == 0 {
			fmt.Fprintf(w, "reset %s\n", scratch)
		}
		ident := identity(c.Author, c.Time, c.TZ)
		msg := c.Message
		if msg != "" && !strings.HasSuffix(msg, "\n") {
			msg += "\n"
		}
		fmt.Fprintf(w, "commit %s\nmark :%d\nauthor %s\ncommitter %s\ndata %d\n%s\n", scratch, i+1, ident, ident, len(msg), msg)
		for k, p := range c.Parents {
			if k == 0 {
				fmt.Fprintf(w, "from %s\n", ref(p))
			} else {
				fmt.Fprintf(w, "merge %s\n", ref(p))
			}
		}
		// Deletions first, so a file can replace a directory and vice versa.
		for _, d := range dels {
			w.WriteString(d)
		}
		for _, m := range mods {
			w.WriteString(m)
		}
		w.WriteString("\n")
	}
	_, err := w.WriteString("done\n")
	return err
}

var identRE = regexp.MustCompile(`^(.*?)\s*<([^<>]*)>\s*$`)

// identity turns a Kudzu author ("Name <email>", or anything else) into a
// git identity line.
func identity(author string, t int64, tz int) string {
	name, email := author, ""
	if m := identRE.FindStringSubmatch(author); m != nil {
		name, email = m[1], m[2]
	}
	clean := func(s string) string {
		return strings.TrimSpace(strings.Map(func(r rune) rune {
			if r == '<' || r == '>' || r == '\n' || r == 0 {
				return -1
			}
			return r
		}, s))
	}
	name, email = clean(name), clean(email)
	sign := '+'
	if tz < 0 {
		sign, tz = '-', -tz
	}
	who := "<" + email + ">"
	if name != "" {
		who = name + " " + who
	}
	return fmt.Sprintf("%s %d %c%02d%02d", who, t, sign, tz/60, tz%60)
}

// quotePath applies git's C-style quoting when fast-import requires it.
func quotePath(p string) string {
	if !strings.ContainsAny(p, "\"\\\n") && !strings.HasPrefix(p, "\"") {
		return p
	}
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(p); i++ {
		switch c := p[i]; c {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func readMarks(path string) (map[int]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	marks := map[int]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		mark, sha, ok := strings.Cut(sc.Text(), " ")
		if !ok || !strings.HasPrefix(mark, ":") {
			continue
		}
		n, err := strconv.Atoi(mark[1:])
		if err != nil {
			continue
		}
		marks[n] = sha
	}
	return marks, sc.Err()
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	var out bytes.Buffer
	err := gitRun(ctx, dir, &out, args...)
	return out.String(), err
}

func gitRun(ctx context.Context, dir string, stdout io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = stdout, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// batchCheck is one long-running `git cat-file --batch-check` process.
type batchCheck struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	w   *bufio.Writer
	out *bufio.Reader
}

func startBatchCheck(gitDir string) (*batchCheck, error) {
	if err := gitRun(context.Background(), gitDir, nil, "rev-parse", "--git-dir"); err != nil {
		return nil, fmt.Errorf("%s is not a git repository: %w", gitDir, err)
	}
	cmd := exec.Command("git", "-C", gitDir, "cat-file", "--batch-check")
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
	return &batchCheck{cmd: cmd, in: in, w: bufio.NewWriter(in), out: bufio.NewReader(out)}, nil
}

func (b *batchCheck) isCommit(sha string) (bool, error) {
	if strings.ContainsAny(sha, " \n") || sha == "" {
		return false, nil
	}
	if _, err := b.w.WriteString(sha + "\n"); err != nil {
		return false, err
	}
	if err := b.w.Flush(); err != nil {
		return false, err
	}
	line, err := b.out.ReadString('\n')
	if err != nil {
		return false, err
	}
	f := strings.Fields(line)
	return len(f) == 3 && f[1] == "commit", nil
}

func (b *batchCheck) close() {
	b.in.Close()
	b.cmd.Wait()
}
