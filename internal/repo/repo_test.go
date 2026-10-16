package repo

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/store"
	"github.com/garyships/kudzu/internal/store/storetest"
)

// newTestRepo uses SQLite and a directory, or the shared backends named by
// KUDZU_TEST_POSTGRES and KUDZU_TEST_S3 (see storetest).
func newTestRepo(t testing.TB) *Repo {
	t.Helper()
	return New(storetest.Meta(t), &store.Cached{Mem: store.NewMem(64 << 20), Backing: storetest.Objects(t)})
}

type files map[string]string

// commit writes the blobs for changes and creates a changeset.
func commit(t testing.TB, r *Repo, parents []object.ID, msg string, set files, del ...string) object.ID {
	t.Helper()
	ctx := context.Background()
	var changes []object.Change
	for p, content := range set {
		id, err := r.WriteBlob(ctx, []byte(content))
		if err != nil {
			t.Fatal(err)
		}
		changes = append(changes, object.Change{Path: p, Mode: object.ModeFile, Blob: id})
	}
	for _, p := range del {
		changes = append(changes, object.Change{Path: p})
	}
	c, err := r.Create(ctx, CreateRequest{Parents: parents, Author: "t <t@example.com>", Time: 1, Message: msg, Changes: changes})
	if err != nil {
		t.Fatalf("create %q: %v", msg, err)
	}
	return c.ID
}

func snapshot(t testing.TB, r *Repo, cs object.ID) files {
	t.Helper()
	ctx := context.Background()
	tree, err := r.TreeOf(ctx, cs)
	if err != nil {
		t.Fatal(err)
	}
	out := files{}
	err = r.WalkTree(ctx, tree, "", func(p string, e object.Entry) error {
		b, err := r.ReadBlob(ctx, e.ID)
		out[p] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func equalFiles(a, b files) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestTreeDerivationMatchesModel(t *testing.T) {
	r := newTestRepo(t)
	rnd := rand.New(rand.NewSource(1))
	model := files{}
	var head object.ID
	paths := []string{"a", "b/c", "b/d/e", "b/d/f", "g/h/i/j", "k"}
	for i := 0; i < 200; i++ {
		set := files{}
		var del []string
		for _, p := range paths {
			switch rnd.Intn(6) {
			case 0:
				set[p] = fmt.Sprintf("%s v%d\n", p, i)
			case 1:
				if _, ok := model[p]; ok {
					del = append(del, p)
				}
			}
		}
		var parents []object.ID
		if !head.IsZero() {
			parents = []object.ID{head}
		}
		head = commit(t, r, parents, fmt.Sprint(i), set, del...)
		for p, c := range set {
			model[p] = c
		}
		for _, p := range del {
			delete(model, p)
		}
		if got := snapshot(t, r, head); !equalFiles(got, model) {
			t.Fatalf("step %d: tree mismatch\ngot  %v\nwant %v", i, got, model)
		}
	}
}

func TestFileDirTransitions(t *testing.T) {
	r := newTestRepo(t)
	c1 := commit(t, r, nil, "file a", files{"a": "1"})
	c2 := commit(t, r, []object.ID{c1}, "a becomes dir", files{"a/b": "2"}, "a")
	if got := snapshot(t, r, c2); !equalFiles(got, files{"a/b": "2"}) {
		t.Fatalf("got %v", got)
	}
	c3 := commit(t, r, []object.ID{c2}, "dir becomes file", files{"a": "3"}, "a/b")
	if got := snapshot(t, r, c3); !equalFiles(got, files{"a": "3"}) {
		t.Fatalf("got %v", got)
	}
	_, err := r.Create(context.Background(), CreateRequest{Parents: []object.ID{c3}, Changes: []object.Change{{Path: "missing"}}})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("deleting a missing file should be invalid, got %v", err)
	}
}

// TestReservedPaths: no changeset may add .git or .kudzu at any depth, but
// one that a repository got before they were reserved can be deleted.
func TestReservedPaths(t *testing.T) {
	r := newTestRepo(t)
	ctx := context.Background()
	c1 := commit(t, r, nil, "base", files{"README": "hi"})
	config, err := r.WriteBlob(ctx, []byte("[core]\n\tfsmonitor = \"touch pwned; false\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	add := []object.Change{{Path: "sub/.git/config", Mode: object.ModeFile, Blob: config}}
	if _, err := r.Create(ctx, CreateRequest{Parents: []object.ID{c1}, Changes: add}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), ".git is reserved") {
		t.Fatalf("adding sub/.git/config: %v", err)
	}
	// An old changeset, appended to the log as it was then.
	raw := (&object.Changeset{Parents: []object.ID{c1}, Author: "a", Time: 1, Message: "old", Changes: add}).Encode()
	old := object.Hash(object.KindChangeset, raw)
	if _, err := r.Meta.AppendChangeset(ctx, old, raw); err != nil {
		t.Fatal(err)
	}
	if got := snapshot(t, r, old); len(got) != 2 {
		t.Fatalf("old changeset has %v", got)
	}
	c3 := commit(t, r, []object.ID{old}, "remove sub/.git", nil, "sub/.git/config")
	if got := snapshot(t, r, c3); !equalFiles(got, files{"README": "hi"}) {
		t.Fatalf("after the delete: %v", got)
	}
}

// randomDAG builds a history with merges and returns changesets in creation order.
func randomDAG(t *testing.T, r *Repo, n int, seed int64) []object.ID {
	rnd := rand.New(rand.NewSource(seed))
	var ids []object.ID
	for i := 0; i < n; i++ {
		var parents []object.ID
		if i > 0 {
			parents = append(parents, ids[len(ids)-1-rnd.Intn(min(len(ids), 4))])
			if i > 3 && rnd.Intn(5) == 0 {
				p := ids[rnd.Intn(len(ids))]
				if p != parents[0] {
					parents = append(parents, p)
				}
			}
		}
		ids = append(ids, commit(t, r, parents, fmt.Sprint(i), files{fmt.Sprintf("f%d", i%7): fmt.Sprint(i)}))
	}
	return ids
}

func ancestors(t *testing.T, r *Repo, id object.ID) map[object.ID]bool {
	seen := map[object.ID]bool{id: true}
	stack := []object.ID{id}
	for len(stack) > 0 {
		x := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		c, err := r.Changeset(context.Background(), x)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range c.Parents {
			if !seen[p] {
				seen[p] = true
				stack = append(stack, p)
			}
		}
	}
	return seen
}

func TestAncestryAgainstBruteForce(t *testing.T) {
	r := newTestRepo(t)
	ctx := context.Background()
	ids := randomDAG(t, r, 120, 3)
	anc := map[object.ID]map[object.ID]bool{}
	for _, id := range ids {
		anc[id] = ancestors(t, r, id)
	}
	rnd := rand.New(rand.NewSource(9))
	for i := 0; i < 600; i++ {
		a, b := ids[rnd.Intn(len(ids))], ids[rnd.Intn(len(ids))]
		got, err := r.IsAncestor(ctx, a, b)
		if err != nil {
			t.Fatal(err)
		}
		if got != anc[b][a] {
			t.Fatalf("IsAncestor(%s, %s) = %v, want %v", a.Short(), b.Short(), got, !got)
		}
		mb, err := r.MergeBase(ctx, a, b)
		if err != nil {
			t.Fatal(err)
		}
		if !anc[a][mb] || !anc[b][mb] {
			t.Fatalf("merge base %s is not a common ancestor", mb.Short())
		}
		// No common ancestor may be a strict descendant of mb.
		for x := range anc[a] {
			if anc[b][x] && x != mb && anc[x][mb] {
				t.Fatalf("merge base %s is not best: %s is a better common ancestor", mb.Short(), x.Short())
			}
		}
	}
}

func TestNthAncestorAndResolve(t *testing.T) {
	r := newTestRepo(t)
	ctx := context.Background()
	var chain []object.ID
	var head object.ID
	for i := 0; i < 300; i++ {
		var parents []object.ID
		if i > 0 {
			parents = []object.ID{head}
		}
		head = commit(t, r, parents, fmt.Sprint(i), files{"x": fmt.Sprint(i)})
		chain = append(chain, head)
	}
	if err := r.Meta.CASRef(ctx, "main", object.Zero, head); err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{0, 1, 2, 3, 7, 64, 100, 255, 299} {
		got, err := r.Resolve(ctx, fmt.Sprintf("main~%d", n))
		if err != nil {
			t.Fatal(err)
		}
		if want := chain[len(chain)-1-n]; got != want {
			t.Fatalf("main~%d = %s, want %s", n, got.Short(), want.Short())
		}
	}
	if _, err := r.Resolve(ctx, "main~300"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected error past root, got %v", err)
	}
	got, err := r.Resolve(ctx, chain[5].String()[:10])
	if err != nil || got != chain[5] {
		t.Fatalf("prefix resolve: %v %v", got, err)
	}
	for rev, want := range map[string]object.ID{
		"main~":                      chain[298],
		"main^":                      chain[298],
		"main^0":                     chain[299],
		"main~~":                     chain[297],
		"main^^^":                    chain[296],
		"main~10~20^~":               chain[299-32],
		chain[5].String()[:10] + "^": chain[4],
	} {
		got, err := r.Resolve(ctx, rev)
		if err != nil || got != want {
			t.Fatalf("%s = %s, %v; want %s", rev, got.Short(), err, want.Short())
		}
	}
	for _, rev := range []string{"main^2", "main~x", "main^-1", "main~1y", "~1", "main~99999999999999999999"} {
		if _, err := r.Resolve(ctx, rev); err == nil {
			t.Fatalf("%s should not resolve", rev)
		}
	}
}

// TestResolveParents checks ^N and chained suffixes on a history with merges
// against a walk of the changesets' parent lists.
func TestResolveParents(t *testing.T) {
	r := newTestRepo(t)
	ctx := context.Background()
	ids := randomDAG(t, r, 80, 5)
	parentsOf := func(id object.ID) []object.ID {
		c, err := r.Changeset(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return c.Parents
	}
	rnd := rand.New(rand.NewSource(11))
	merges := 0
	for i := 0; i < 400; i++ {
		start := ids[rnd.Intn(len(ids))]
		rev, want := start.String()[:16], start
		for k := rnd.Intn(4); k >= 0; k-- {
			ps := parentsOf(want)
			if len(ps) == 0 {
				break
			}
			if rnd.Intn(2) == 0 {
				n := 1 + rnd.Intn(len(ps))
				if n == 1 && rnd.Intn(2) == 0 {
					rev += "^"
				} else {
					rev += fmt.Sprintf("^%d", n)
				}
				merges += n - 1
				want = ps[n-1]
				continue
			}
			n := 0
			for limit := 1 + rnd.Intn(3); n < limit && len(parentsOf(want)) > 0; n++ {
				want = parentsOf(want)[0]
			}
			if n == 1 && rnd.Intn(2) == 0 {
				rev += "~"
			} else {
				rev += fmt.Sprintf("~%d", n)
			}
		}
		got, err := r.Resolve(ctx, rev)
		if err != nil {
			t.Fatalf("%s: %v", rev, err)
		}
		if got != want {
			t.Fatalf("%s = %s, want %s", rev, got.Short(), want.Short())
		}
	}
	if merges == 0 {
		t.Fatal("test never followed a second parent")
	}
}

func TestPathLogAndBlame(t *testing.T) {
	r := newTestRepo(t)
	ctx := context.Background()
	c1 := commit(t, r, nil, "one", files{"src/a.txt": "l1\nl2\nl3\n", "other": "x"})
	c2 := commit(t, r, []object.ID{c1}, "two", files{"other": "y"})
	c3 := commit(t, r, []object.ID{c2}, "three", files{"src/a.txt": "l1\nL2\nl3\nl4\n"})
	c4 := commit(t, r, []object.ID{c3}, "four", files{"other": "z"})

	log, err := r.PathLog(ctx, c4, "src/a.txt", 10)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(log) != fmt.Sprint([]object.ID{c3, c1}) {
		t.Fatalf("path log = %v", log)
	}
	dirLog, err := r.PathLog(ctx, c4, "src", 10)
	if err != nil || fmt.Sprint(dirLog) != fmt.Sprint([]object.ID{c3, c1}) {
		t.Fatalf("dir log = %v %v", dirLog, err)
	}
	bl, err := r.Blame(ctx, c4, "src/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	want := []object.ID{c1, c3, c1, c3}
	for i, l := range bl {
		if l.Changeset != want[i] {
			t.Fatalf("line %d blamed on %s, want %s", i+1, l.Changeset.Short(), want[i].Short())
		}
	}
}

func TestRebaseAndMerge(t *testing.T) {
	r := newTestRepo(t)
	ctx := context.Background()
	base := commit(t, r, nil, "base", files{"f": "a\nb\nc\nd\ne\n", "g": "g\n"})
	upstream := commit(t, r, []object.ID{base}, "upstream", files{"f": "A\nb\nc\nd\ne\n", "new": "n\n"})
	mine := commit(t, r, []object.ID{base}, "mine", files{"f": "a\nb\nc\nd\nE\n"}, "g")

	rebased, err := r.Rebase(ctx, mine, upstream)
	if err != nil {
		t.Fatal(err)
	}
	want := files{"f": "A\nb\nc\nd\nE\n", "new": "n\n"}
	if got := snapshot(t, r, rebased); !equalFiles(got, want) {
		t.Fatalf("rebased tree = %v", got)
	}
	merged, err := r.Merge(ctx, upstream, mine, "t", "merge")
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot(t, r, merged.ID); !equalFiles(got, want) {
		t.Fatalf("merged tree = %v", got)
	}
	conflicting := commit(t, r, []object.ID{base}, "conflict", files{"f": "X\nb\nc\nd\ne\n"})
	_, err = r.Rebase(ctx, conflicting, upstream)
	var ce *ConflictError
	if !errors.As(err, &ce) || len(ce.Paths) != 1 || ce.Paths[0] != "f" {
		t.Fatalf("expected conflict on f, got %v", err)
	}
}

// TestConcurrentLand lands many changesets at once with no global lock. Every
// one must end up on main exactly once, and main must stay linear.
func TestConcurrentLand(t *testing.T) {
	r := newTestRepo(t)
	ctx := context.Background()
	root := commit(t, r, nil, "root", files{"README": "hi\n"})
	if err := r.Meta.CASRef(ctx, "main", object.Zero, root); err != nil {
		t.Fatal(err)
	}
	const n = 40
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := commit(t, r, []object.ID{root}, fmt.Sprintf("change %d", i), files{fmt.Sprintf("dir%d/file", i%5) + fmt.Sprint(i): fmt.Sprint(i)})
			if _, err := r.Land(ctx, c, "main"); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	tip, _ := r.Meta.GetRef(ctx, "main")
	log, err := r.Log(ctx, tip, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != n+1 {
		t.Fatalf("main has %d changesets, want %d", len(log), n+1)
	}
	var msgs []string
	for _, id := range log[:n] {
		c, _ := r.Changeset(ctx, id)
		if len(c.Parents) != 1 {
			t.Fatal("main is not linear")
		}
		msgs = append(msgs, c.Message)
	}
	sort.Strings(msgs)
	for i := 1; i < len(msgs); i++ {
		if msgs[i] == msgs[i-1] {
			t.Fatalf("%q landed twice", msgs[i])
		}
	}
	if got := len(snapshot(t, r, tip)); got != n+1 {
		t.Fatalf("final tree has %d files, want %d", got, n+1)
	}
	if !strings.HasPrefix(msgs[0], "change") {
		t.Fatal("unexpected message")
	}
}

func TestLazyDerivation(t *testing.T) {
	r := newTestRepo(t)
	r.Eager = nil // nothing but trees computed up front
	ctx := context.Background()
	var head object.ID
	for i := 0; i < 50; i++ {
		var parents []object.ID
		if i > 0 {
			parents = []object.ID{head}
		}
		head = commit(t, r, parents, fmt.Sprint(i), files{fmt.Sprintf("d/f%d", i%3): fmt.Sprint(i)})
	}
	if _, err := r.Meta.GetDerived(ctx, "unode", head); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("unode should not be derived yet")
	}
	log, err := r.PathLog(ctx, head, "d/f1", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 17 {
		t.Fatalf("path log has %d entries, want 17", len(log))
	}
}
