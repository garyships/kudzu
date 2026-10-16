package object

import (
	"bytes"
	"strings"
	"testing"
)

func TestEncodeRoundTrip(t *testing.T) {
	for _, content := range [][]byte{nil, []byte("x"), bytes.Repeat([]byte("abc"), 1000)} {
		id, stored := EncodeObject(KindBlob, content)
		kind, got, err := Verify(id, stored)
		if err != nil || kind != KindBlob || !bytes.Equal(got, content) {
			t.Fatalf("round trip failed: %v", err)
		}
		stored[len(stored)-1] ^= 1
		if _, _, err := Verify(id, stored); err == nil {
			t.Fatal("corruption not detected")
		}
	}
}

func TestTreeRoundTrip(t *testing.T) {
	tr := &Tree{Entries: []Entry{
		{Name: "a.txt", Mode: ModeFile, ID: Hash(KindBlob, []byte("a"))},
		{Name: "bin", Mode: ModeDir, ID: EmptyTreeID},
		{Name: "run", Mode: ModeExec, ID: Hash(KindBlob, []byte("r"))},
	}}
	got, err := DecodeTree(tr.Encode())
	if err != nil || len(got.Entries) != 3 || got.Entries[1] != tr.Entries[1] {
		t.Fatalf("tree round trip: %v %+v", err, got)
	}
	if e, ok := got.Lookup("run"); !ok || e.Mode != ModeExec {
		t.Fatal("lookup failed")
	}
}

func TestHostileTreeNames(t *testing.T) {
	for _, name := range []string{"..", ".", "a/b", "", "x\x00y"} {
		var b bytes.Buffer
		encodeEntries(&b, []Entry{{Name: name, Mode: ModeFile}})
		if _, err := DecodeTree(b.Bytes()); err == nil && !strings.Contains(name, "\x00") {
			t.Fatalf("name %q accepted", name)
		}
	}
}

func TestUnodeRoundTrip(t *testing.T) {
	u := &Unode{Linknode: Hash(KindChangeset, []byte("c")), Parents: []ID{Hash(KindUnode, []byte("p"))}, Mode: ModeFile, Content: Hash(KindBlob, []byte("x"))}
	got, err := DecodeUnode(u.Encode())
	if err != nil || got.Linknode != u.Linknode || got.Content != u.Content || len(got.Parents) != 1 {
		t.Fatalf("unode round trip: %v", err)
	}
	d := &Unode{Dir: true, Linknode: u.Linknode, Entries: []Entry{{Name: "f", Mode: ModeFile, ID: Hash(KindUnode, []byte("f"))}}}
	got, err = DecodeUnode(d.Encode())
	if err != nil || !got.Dir || len(got.Entries) != 1 || len(got.Parents) != 0 {
		t.Fatalf("dir unode round trip: %v", err)
	}
}

func TestValidatePath(t *testing.T) {
	for _, p := range []string{"a", "a/b.txt", ".github/x", ".kudzuignore", "a/.kudzuignore", ".gitignore", "a/.gitkeep", "a/.github/x", "git", "a/git~2", "x.git", ".git-blame-ignore-revs", "kudzu"} {
		if err := ValidatePath(p); err != nil {
			t.Fatalf("%q rejected: %v", p, err)
		}
	}
	for _, p := range []string{"", "/a", "a/", "a//b", "../x", "a/./b", ".kudzu/state", ".git/config", "sub/.git/config", "a/b/.git",
		".GIT/config", "sub/.Git/HEAD", "sub/.git./config", "sub/.git /config", "sub/.git. ./config", "GIT~1/config", "sub/git~1/config",
		"x/.kudzu/state.json", ".KUDZU/index", ".kudzu./x", "sub/kudzu~1/x"} {
		if ValidatePath(p) == nil {
			t.Fatalf("%q accepted", p)
		}
	}
}
