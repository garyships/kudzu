package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/user"
	"sort"
	"strings"
	"time"

	"github.com/garyships/kudzu/internal/client"
	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/proto"
	"github.com/garyships/kudzu/internal/repo"
	"github.com/garyships/kudzu/internal/sparse"
	"github.com/garyships/kudzu/internal/vfs"
)

// inMount returns the writable mount (kudzu mount --writable) the current
// directory is in, if any.
func inMount() (*vfs.MountClient, bool) { return vfs.FindMount(".") }

// cwdWC is the working copy containing the current directory, opened on
// first use and at most once per command: opening one loads its whole
// index, and a query command may need it for both its remote and its
// default revision.
type cwdWC struct {
	wc     *client.WC
	opened bool
}

func (c *cwdWC) get() *client.WC {
	if !c.opened {
		c.opened = true
		c.wc, _ = client.Open(".")
	}
	return c.wc
}

// dialRemote picks the remote from the flag, $KUDZU_REMOTE, the writable
// mount or the working copy.
func dialRemote(flagValue string) (*proto.Remote, error) {
	return new(cwdWC).dialRemote(flagValue)
}

func (c *cwdWC) dialRemote(flagValue string) (*proto.Remote, error) {
	if flagValue != "" {
		return proto.Dial(flagValue)
	}
	if env := os.Getenv("KUDZU_REMOTE"); env != "" {
		return proto.Dial(env)
	}
	if m, ok := inMount(); ok {
		return proto.Dial(m.Info.Remote)
	}
	if wc := c.get(); wc != nil {
		return wc.Remote, nil
	}
	return nil, errors.New("no remote: pass --remote, set KUDZU_REMOTE, or run inside a working copy")
}

// defaultRev is the checked-out changeset when inside a working copy or a
// writable mount, else main.
func (c *cwdWC) defaultRev(rev string) string {
	if rev != "" {
		return rev
	}
	if m, ok := inMount(); ok {
		if id, err := m.Base(context.Background()); err == nil {
			return id.String()
		}
	}
	if wc := c.get(); wc != nil && !wc.State.Changeset.IsZero() {
		return wc.State.Changeset.String()
	}
	return "main"
}

func defaultAuthor(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if env := os.Getenv("KUDZU_AUTHOR"); env != "" {
		return env
	}
	name := "unknown"
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	host, _ := os.Hostname()
	return fmt.Sprintf("%s <%s@%s>", name, name, host)
}

func openWC() (*client.WC, error) {
	if m, ok := inMount(); ok {
		return nil, fmt.Errorf("%s is a writable kudzu mount, not a working copy: status, diff, commit, update and land work here, this command does not", m.Info.Root)
	}
	wc, err := client.Open(".")
	if err != nil {
		return nil, err
	}
	wc.Out = os.Stderr
	return wc, nil
}

func splitList(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

func cmdClone(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("clone", flag.ContinueOnError)
	rev := fs.String("r", "main", "revision to check out")
	sparseDirs := fs.String("sparse", "", "comma-separated directories to check out (default: everything)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return errors.New("usage: kudzu clone [-r REV] [--sparse a,b] REMOTE DIR")
	}
	sp, err := sparse.Normalize(splitList(*sparseDirs))
	if err != nil {
		return err
	}
	start := time.Now()
	wc, err := client.Clone(ctx, pos[0], pos[1], *rev, sp, os.Stderr)
	if err != nil {
		return err
	}
	fmt.Printf("checked out %s (%s) into %s in %s\n", wc.State.Changeset.Short(), plural(len(wc.Index), "file", "files"), pos[1], time.Since(start).Round(time.Millisecond))
	return nil
}

func cmdStatus(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	addJSONFlag(fs)
	if _, err := parse(fs, args); err != nil {
		return err
	}
	if m, ok := inMount(); ok {
		st, err := m.Status(ctx)
		if err != nil {
			return err
		}
		if jsonOutput {
			out := statusJSON{Changeset: st.Changeset, Files: []fileStatusJSON{}}
			for _, f := range st.Files {
				out.Files = append(out.Files, fileStatusJSON{f.Path, f.Status})
			}
			return printJSON(out)
		}
		for _, f := range st.Files {
			fmt.Printf("%s %s\n", f.Status, f.Path)
		}
		return nil
	}
	wc, err := openWC()
	if err != nil {
		return err
	}
	st, err := wc.Status(ctx)
	if err != nil {
		return err
	}
	if jsonOutput {
		out := statusJSON{Changeset: wc.State.Changeset, Files: []fileStatusJSON{}}
		for _, f := range st {
			out.Files = append(out.Files, fileStatusJSON{f.Path, string(f.Code)})
		}
		if err := printJSON(out); err != nil {
			return err
		}
	} else {
		for _, f := range st {
			fmt.Printf("%c %s\n", f.Code, f.Path)
		}
	}
	return wc.Save() // persist refreshed stat data
}

func cmdCommit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("commit", flag.ContinueOnError)
	msg := fs.String("m", "", "message")
	addAll := fs.Bool("A", false, "also add untracked files")
	land := fs.String("land", "", "land the new changeset on this ref in the same call")
	author := fs.String("author", "", "author (default $KUDZU_AUTHOR or user@host)")
	addJSONFlag(fs)
	if _, err := parse(fs, args); err != nil {
		return err
	}
	if *msg == "" {
		return errors.New("a message is required (-m)")
	}
	if m, ok := inMount(); ok {
		res, err := m.Commit(ctx, *msg, defaultAuthor(*author), *land)
		if err != nil {
			return err
		}
		if jsonOutput {
			out := struct {
				ID      object.ID `json:"id"`
				Changes int       `json:"changes"`
				Land    *landJSON `json:"land,omitempty"`
			}{ID: res.ID, Changes: res.Changes}
			if res.Land != nil {
				out.Land = &landJSON{*land, res.Land}
			}
			return printJSON(out)
		}
		fmt.Printf("created %s (%s)\n", res.ID.Short(), plural(res.Changes, "file", "files"))
		if res.Land != nil {
			printLand(res.Land, *land)
		}
		return nil
	}
	wc, err := openWC()
	if err != nil {
		return err
	}
	res, err := wc.Commit(ctx, *msg, defaultAuthor(*author), *addAll, *land)
	if err != nil {
		return err
	}
	if jsonOutput {
		out := struct {
			ID      object.ID `json:"id"`
			Tree    object.ID `json:"tree"`
			Changes int       `json:"changes"`
			Land    *landJSON `json:"land,omitempty"`
		}{ID: res.ID, Tree: res.Tree, Changes: res.Changes}
		if res.Land != nil {
			out.Land = &landJSON{*land, res.Land}
		}
		return printJSON(out)
	}
	fmt.Printf("created %s (%s)\n", res.ID.Short(), plural(res.Changes, "file", "files"))
	if res.Land != nil {
		printLand(res.Land, *land)
	}
	return nil
}

func printLand(res *repo.LandResult, ref string) {
	how := "fast-forward"
	if res.Rebased {
		how = "rebased"
	}
	fmt.Printf("landed %s on %s (%s, %s)\n", res.Landed.Short(), ref, how, plural(res.Attempts, "attempt", "attempts"))
}

func cmdUpdate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	rev := fs.String("r", "main", "revision")
	force := fs.Bool("force", false, "discard local changes")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 1 {
		*rev = pos[0]
	}
	if m, ok := inMount(); ok {
		id, err := m.Update(ctx, *rev, *force)
		if err != nil {
			return err
		}
		fmt.Printf("at %s\n", id.Short())
		return nil
	}
	wc, err := openWC()
	if err != nil {
		return err
	}
	if err := wc.Update(ctx, *rev, *force); err != nil {
		return err
	}
	fmt.Printf("at %s\n", wc.State.Changeset.Short())
	return nil
}

func cmdRebase(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("rebase", flag.ContinueOnError)
	onto := fs.String("d", "main", "destination revision")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	wc, err := openWC()
	if err != nil {
		return err
	}
	head, err := wc.Rebase(ctx, *onto)
	if err != nil {
		return err
	}
	fmt.Printf("rebased onto %s; now at %s\n", *onto, head.Short())
	return nil
}

func cmdLand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("land", flag.ContinueOnError)
	ref := fs.String("ref", "main", "ref to land on")
	addJSONFlag(fs)
	if _, err := parse(fs, args); err != nil {
		return err
	}
	if m, ok := inMount(); ok {
		res, err := m.Land(ctx, *ref)
		if err != nil {
			return err
		}
		if jsonOutput {
			return printJSON(landJSON{*ref, res})
		}
		printLand(res, *ref)
		return nil
	}
	wc, err := openWC()
	if err != nil {
		return err
	}
	res, err := wc.Land(ctx, *ref)
	if err != nil {
		return err
	}
	if jsonOutput {
		return printJSON(landJSON{*ref, res})
	}
	printLand(res, *ref)
	return nil
}

func cmdSparse(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sparse", flag.ContinueOnError)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	wc, err := openWC()
	if err != nil {
		return err
	}
	if len(pos) == 0 || pos[0] == "show" {
		if wc.State.Sparse.Full() {
			fmt.Println("(full checkout)")
		}
		for _, d := range wc.State.Sparse {
			fmt.Println(d)
		}
		return nil
	}
	cur := append([]string(nil), wc.State.Sparse...)
	var next []string
	switch pos[0] {
	case "set":
		next = pos[1:]
	case "add":
		if len(cur) == 0 {
			fmt.Println("already a full checkout")
			return nil
		}
		next = append(cur, pos[1:]...)
	case "remove":
		drop := map[string]bool{}
		for _, d := range pos[1:] {
			drop[strings.Trim(d, "/")] = true
		}
		for _, d := range cur {
			if !drop[d] {
				next = append(next, d)
			}
		}
		if len(next) == 0 {
			return errors.New("refusing to remove every sparse directory (use 'sparse set .' for a full checkout)")
		}
	default:
		return fmt.Errorf("unknown sparse subcommand %q (show, set, add, remove)", pos[0])
	}
	sp, err := sparse.Normalize(next)
	if err != nil {
		return err
	}
	if err := wc.SetSparse(ctx, sp); err != nil {
		return err
	}
	fmt.Printf("%s checked out\n", plural(len(wc.Index), "file", "files"))
	return nil
}

func cmdGC(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	max := fs.String("max-size", "1GB", "target size of the local object cache")
	stores := addStoreFlags(fs, "")
	keep := fs.String("keep", "30d", "with --store/--meta/--objects: grace period; anything newer is kept (e.g. 12h, 7d, 2w)")
	dryRun := fs.Bool("dry-run", false, "with --store/--meta/--objects: report what would be removed without removing it")
	addJSONFlag(fs)
	if _, err := parse(fs, args); err != nil {
		return err
	}
	// With a server store, collect its garbage instead of the working copy's.
	if stores.set() {
		return storeGC(ctx, stores, *keep, *dryRun)
	}
	n, err := parseSize(*max)
	if err != nil {
		return err
	}
	wc, err := openWC()
	if err != nil {
		return err
	}
	removed, err := wc.Objs.GC(n)
	if err != nil {
		return err
	}
	if jsonOutput {
		return printJSON(map[string]int{"removed_objects": removed})
	}
	fmt.Printf("removed %s\n", plural(removed, "cached object", "cached objects"))
	return nil
}

func storeGC(ctx context.Context, stores storeFlags, keep string, dryRun bool) error {
	grace, err := parseDuration(keep)
	if err != nil {
		return err
	}
	if grace < time.Hour {
		fmt.Fprintln(os.Stderr, "warning: a grace period under 1h is only safe when no server is using the store")
	}
	r, closeRepo, err := stores.open(ctx, 64<<20, true)
	if err != nil {
		return err
	}
	defer closeRepo()
	res, err := r.GC(ctx, repo.GCOptions{Keep: grace, DryRun: dryRun})
	if err != nil {
		return err
	}
	if jsonOutput {
		return printJSON(res)
	}
	verb := "removed"
	if dryRun {
		verb = "would remove"
	}
	fmt.Printf("%s %s of %d and %s of %d (%s)\n", verb,
		plural(res.RemovedChangesets, "changeset", "changesets"), res.Changesets,
		plural(res.RemovedObjects, "object", "objects"), res.Objects, formatBytes(res.RemovedBytes))
	return nil
}

type changesetView struct {
	ID          object.ID         `json:"id"`
	Seq         int64             `json:"seq"`
	Tree        object.ID         `json:"tree"`
	Parents     []object.ID       `json:"parents"`
	Author      string            `json:"author"`
	Time        int64             `json:"time"`
	TZ          int               `json:"tz"`
	Message     string            `json:"message"`
	Changes     []object.Change   `json:"changes"`
	Extra       map[string]string `json:"extra"`
	ChangeCount int               `json:"change_count"`
}

func formatTime(t int64, tz int) string {
	return time.Unix(t, 0).In(time.FixedZone("", tz*60)).Format("2006-01-02 15:04:05 -0700")
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}

func printChangeset(c changesetView, oneline bool) {
	if oneline {
		fmt.Printf("%s %s\n", c.ID.Short(), firstLine(c.Message))
		return
	}
	fmt.Printf("changeset %s\n", c.ID)
	if len(c.Parents) > 1 {
		var ps []string
		for _, p := range c.Parents {
			ps = append(ps, p.Short())
		}
		fmt.Printf("Merge:  %s\n", strings.Join(ps, " "))
	}
	fmt.Printf("Author: %s\nDate:   %s\n", c.Author, formatTime(c.Time, c.TZ))
	if g := c.Extra["git"]; g != "" {
		fmt.Printf("Git:    %s\n", g)
	}
	fmt.Println()
	for _, l := range strings.Split(c.Message, "\n") {
		fmt.Printf("    %s\n", l)
	}
	fmt.Println()
}

func cmdLog(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("log", flag.ContinueOnError)
	remote := fs.String("remote", "", "remote address")
	rev := fs.String("r", "", "revision (default: working copy, else main)")
	n := fs.Int("n", 20, "max changesets")
	oneline := fs.Bool("oneline", false, "one line per changeset")
	addJSONFlag(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	var cwd cwdWC
	rem, err := cwd.dialRemote(*remote)
	if err != nil {
		return err
	}
	q := url.Values{"rev": {cwd.defaultRev(*rev)}, "limit": {fmt.Sprint(*n)}}
	if len(pos) > 0 {
		q.Set("path", pos[0])
	}
	var out []changesetView
	if done, err := query(ctx, rem, "/api/log", q, &out); err != nil || done {
		return err
	}
	for _, c := range out {
		printChangeset(c, *oneline)
	}
	return nil
}

func cmdBlame(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("blame", flag.ContinueOnError)
	remote := fs.String("remote", "", "remote address")
	rev := fs.String("r", "", "revision (default: working copy, else main)")
	addJSONFlag(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: kudzu blame [-r REV] PATH")
	}
	var cwd cwdWC
	rem, err := cwd.dialRemote(*remote)
	if err != nil {
		return err
	}
	var lines []struct {
		Line      int       `json:"line"`
		Changeset object.ID `json:"changeset"`
		Author    string    `json:"author"`
		Time      int64     `json:"time"`
		Text      string    `json:"text"`
	}
	if done, err := query(ctx, rem, "/api/blame", url.Values{"rev": {cwd.defaultRev(*rev)}, "path": {pos[0]}}, &lines); err != nil || done {
		return err
	}
	for _, l := range lines {
		author, _, _ := strings.Cut(l.Author, " <")
		fmt.Printf("%s %s %s %4d) %s", l.Changeset.Short(), fitWidth(author, 16), time.Unix(l.Time, 0).UTC().Format("2006-01-02"), l.Line, l.Text)
		if !strings.HasSuffix(l.Text, "\n") {
			fmt.Println()
		}
	}
	return nil
}

func cmdDiff(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	remote := fs.String("remote", "", "remote address")
	from := fs.String("from", "", "old revision (default: parent of --to)")
	to := fs.String("to", "", "new revision")
	mergeBase := fs.Bool("merge-base", false, "diff from the merge base of --from and --to")
	stat := fs.Bool("stat", false, "list changed files only")
	addJSONFlag(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if *from == "" && *to == "" {
		if m, ok := inMount(); ok {
			files, patch, err := m.Diff(ctx)
			if err != nil {
				return err
			}
			if jsonOutput {
				out := struct {
					Files []fileStatusJSON `json:"files"`
					Patch string           `json:"patch,omitempty"`
				}{Files: []fileStatusJSON{}}
				for _, f := range files {
					out.Files = append(out.Files, fileStatusJSON{f.Path, f.Status})
				}
				if !*stat {
					out.Patch = patch
				}
				return printJSON(out)
			}
			if *stat {
				for _, f := range files {
					fmt.Printf("%s %s\n", f.Status, f.Path)
				}
				return nil
			}
			fmt.Print(patch)
			return nil
		}
		if wc, err := openWC(); err == nil {
			if jsonOutput {
				return localDiffJSON(ctx, wc, *stat)
			}
			if *stat {
				st, err := wc.Status(ctx)
				if err != nil {
					return err
				}
				for _, f := range st {
					if f.Code != '?' {
						fmt.Printf("%c %s\n", f.Code, f.Path)
					}
				}
				return nil
			}
			out, err := wc.LocalDiff(ctx)
			fmt.Print(out)
			return err
		}
		*to = "main"
	}
	var cwd cwdWC
	rem, err := cwd.dialRemote(*remote)
	if err != nil {
		return err
	}
	q := url.Values{"to": {cwd.defaultRev(*to)}}
	if *from != "" {
		q.Set("from", *from)
	}
	if *mergeBase {
		q.Set("merge_base", "1")
	}
	if !*stat {
		q.Set("patch", "1")
	}
	if len(pos) > 0 {
		q.Set("path", pos[0])
	}
	var resp struct {
		Files []repo.FileDiff `json:"files"`
		Patch string          `json:"patch"`
	}
	if done, err := query(ctx, rem, "/api/diff", q, &resp); err != nil || done {
		return err
	}
	if *stat {
		for _, f := range resp.Files {
			code := 'M'
			if f.Old == nil {
				code = 'A'
			} else if f.New == nil {
				code = 'D'
			}
			fmt.Printf("%c %s\n", code, f.Path)
		}
		return nil
	}
	fmt.Print(resp.Patch)
	return nil
}

func cmdShow(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	remote := fs.String("remote", "", "remote address")
	addJSONFlag(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	rev := ""
	if len(pos) > 0 {
		rev = pos[0]
	}
	var cwd cwdWC
	rem, err := cwd.dialRemote(*remote)
	if err != nil {
		return err
	}
	var c changesetView
	if done, err := query(ctx, rem, "/api/changesets/"+url.PathEscape(cwd.defaultRev(rev)), nil, &c); err != nil || done {
		return err
	}
	printChangeset(c, false)
	fmt.Printf("seq %d, tree %s\n", c.Seq, c.Tree.Short())
	for _, ch := range c.Changes {
		if ch.Deleted() {
			fmt.Printf("D %s\n", ch.Path)
		} else {
			fmt.Printf("%o %s %s\n", ch.Mode, ch.Blob.Short(), ch.Path)
		}
	}
	return nil
}

func cmdCat(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("cat", flag.ContinueOnError)
	remote := fs.String("remote", "", "remote address")
	rev := fs.String("r", "", "revision")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: kudzu cat [-r REV] PATH")
	}
	var cwd cwdWC
	rem, err := cwd.dialRemote(*remote)
	if err != nil {
		return err
	}
	b, err := rem.GetRaw(ctx, "/api/cat", url.Values{"rev": {cwd.defaultRev(*rev)}, "path": {pos[0]}})
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(b)
	return err
}

func cmdLs(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	remote := fs.String("remote", "", "remote address")
	rev := fs.String("r", "", "revision")
	addJSONFlag(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	p := ""
	if len(pos) > 0 {
		p = pos[0]
	}
	var cwd cwdWC
	rem, err := cwd.dialRemote(*remote)
	if err != nil {
		return err
	}
	var entries []object.Entry
	if done, err := query(ctx, rem, "/api/ls", url.Values{"rev": {cwd.defaultRev(*rev)}, "path": {p}}, &entries); err != nil || done {
		return err
	}
	for _, e := range entries {
		name := e.Name
		if e.IsDir() {
			name += "/"
		}
		fmt.Printf("%06o %s %s\n", e.Mode, e.ID.Short(), name)
	}
	return nil
}

func cmdRefs(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("refs", flag.ContinueOnError)
	remote := fs.String("remote", "", "remote address")
	addJSONFlag(fs)
	if _, err := parse(fs, args); err != nil {
		return err
	}
	rem, err := dialRemote(*remote)
	if err != nil {
		return err
	}
	var refs map[string]object.ID
	if done, err := query(ctx, rem, "/api/refs", nil, &refs); err != nil || done {
		return err
	}
	names := make([]string, 0, len(refs))
	for name := range refs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Printf("%s %s\n", refs[name].Short(), name)
	}
	return nil
}

func cmdMergeBase(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("merge-base", flag.ContinueOnError)
	remote := fs.String("remote", "", "remote address")
	addJSONFlag(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return errors.New("usage: kudzu merge-base A B")
	}
	rem, err := dialRemote(*remote)
	if err != nil {
		return err
	}
	var resp struct {
		MergeBase object.ID `json:"merge_base"`
	}
	if done, err := query(ctx, rem, "/api/ancestry", url.Values{"a": {pos[0]}, "b": {pos[1]}}, &resp); err != nil || done {
		return err
	}
	if resp.MergeBase.IsZero() {
		return errors.New("no common ancestor")
	}
	fmt.Println(resp.MergeBase)
	return nil
}

// cmdWrite creates a changeset purely through the API: no working copy, no
// checkout. Arguments are PATH=@LOCALFILE (or PATH=@- for stdin).
func cmdWrite(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("write", flag.ContinueOnError)
	remote := fs.String("remote", "", "remote address")
	rev := fs.String("r", "main", "parent revision")
	msg := fs.String("m", "", "message")
	land := fs.String("land", "", "land on this ref in the same call")
	author := fs.String("author", "", "author")
	var deletes multiFlag
	fs.Var(&deletes, "delete", "path to delete (repeatable)")
	addJSONFlag(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if *msg == "" {
		return errors.New("a message is required (-m)")
	}
	rem, err := dialRemote(*remote)
	if err != nil {
		return err
	}
	var parent struct {
		Changeset object.ID `json:"changeset"`
	}
	if err := rem.GetJSON(ctx, "/api/resolve", url.Values{"rev": {*rev}}, &parent); err != nil {
		return err
	}
	var changes []object.Change
	uploads := map[object.ID][]byte{}
	for _, a := range pos {
		p, src, ok := strings.Cut(a, "=@")
		if !ok {
			return fmt.Errorf("expected PATH=@FILE, got %q", a)
		}
		var data []byte
		if src == "-" {
			data, err = readAllStdin()
		} else {
			data, err = os.ReadFile(src)
		}
		if err != nil {
			return err
		}
		mode := object.ModeFile
		if info, err := os.Stat(src); err == nil && info.Mode().Perm()&0o111 != 0 {
			mode = object.ModeExec
		}
		id, stored := object.EncodeObject(object.KindBlob, data)
		uploads[id] = stored
		changes = append(changes, object.Change{Path: p, Mode: mode, Blob: id})
	}
	for _, p := range deletes {
		changes = append(changes, object.Change{Path: p})
	}
	ids := make([]object.ID, 0, len(uploads))
	for id := range uploads {
		ids = append(ids, id)
	}
	missing, err := rem.Missing(ctx, ids)
	if err != nil {
		return err
	}
	send := map[object.ID][]byte{}
	for _, id := range missing {
		send[id] = uploads[id]
	}
	if err := rem.Upload(ctx, send); err != nil {
		return err
	}
	req := map[string]any{
		"parents": []object.ID{parent.Changeset},
		"author":  defaultAuthor(*author),
		"message": *msg,
		"changes": changes,
	}
	if *land != "" {
		req["land"] = *land
	}
	var resp struct {
		ID   object.ID        `json:"id"`
		Tree object.ID        `json:"tree"`
		Seq  int64            `json:"seq"`
		Land *repo.LandResult `json:"land"`
	}
	if err := rem.PostJSON(ctx, "/api/changesets", req, &resp); err != nil {
		return err
	}
	if jsonOutput {
		out := struct {
			ID       object.ID `json:"id"`
			Tree     object.ID `json:"tree"`
			Seq      int64     `json:"seq"`
			Files    int       `json:"files"`
			Uploaded int       `json:"uploaded"`
			Land     *landJSON `json:"land,omitempty"`
		}{resp.ID, resp.Tree, resp.Seq, len(changes), len(send), nil}
		if resp.Land != nil {
			out.Land = &landJSON{*land, resp.Land}
		}
		return printJSON(out)
	}
	fmt.Printf("created %s (%s, %s uploaded)\n", resp.ID.Short(), plural(len(changes), "file", "files"), plural(len(send), "blob", "blobs"))
	if resp.Land != nil {
		printLand(resp.Land, *land)
	}
	return nil
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func readAllStdin() ([]byte, error) { return io.ReadAll(os.Stdin) }
