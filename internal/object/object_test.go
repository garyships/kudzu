package object

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"hash/adler32"
	"io"
	"runtime"
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

// sourceBlob is about 3 KB of compressible source code.
var sourceBlob = bytes.Repeat([]byte("package main\n\nfunc main() { println(\"hello\") }\n"), 60)

// TestDecodeReusesReaders: decoding a compressed blob allocates about its
// content, not a new zlib reader (~40 KB, mostly its window) every time.
func TestDecodeReusesReaders(t *testing.T) {
	stored := Encode(KindBlob, sourceBlob)
	if stored[1] != codecZlib {
		t.Fatal("test blob was not compressed")
	}
	const runs = 1000
	Decode(stored)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for i := 0; i < runs; i++ {
		if _, _, err := Decode(stored); err != nil {
			t.Fatal(err)
		}
	}
	runtime.ReadMemStats(&after)
	// About 7.5 KB; the race detector makes sync.Pool drop a quarter of
	// what is put back, which brings it to about 20 KB.
	if per := (after.TotalAlloc - before.TotalAlloc) / runs; per > 32<<10 {
		t.Fatalf("Decode allocated %d bytes per %d-byte blob", per, len(sourceBlob))
	}
}

// TestSizeLimit: content past the limit is an error for every decoder,
// compressed or not, and decoding stops at the limit instead of reading the
// whole payload into memory first.
func TestSizeLimit(t *testing.T) {
	const limit = 1 << 20
	for _, n := range []int{limit - 1, limit, limit + 1, 8 * limit} {
		content := make([]byte, n)
		id := Hash(KindBlob, content)
		raw := append([]byte{byte(KindBlob), codecRaw}, content...)
		for _, stored := range [][]byte{Encode(KindBlob, content), raw} {
			_, got, err := DecodeLimit(stored, limit)
			_, _, verr := verify(id, stored, limit)
			_, cerr := check(id, stored, limit)
			if n > limit {
				if !errors.Is(err, ErrTooLarge) || verr == nil || cerr == nil {
					t.Fatalf("%d bytes, codec %d: accepted past a %d-byte limit: %v %v %v", n, stored[1], limit, err, verr, cerr)
				}
				continue
			}
			if err != nil || verr != nil || cerr != nil || len(got) != n {
				t.Fatalf("%d bytes, codec %d: %v %v %v", n, stored[1], err, verr, cerr)
			}
			if c := cap(got); stored[1] == codecZlib && c > limit+64<<10 {
				t.Fatalf("%d bytes decoded into a %d-byte buffer", n, c)
			}
		}
	}
}

// TestCheckDoesNotInflateIntoMemory: a small payload that inflates to 16 MiB
// costs Check CPU, not memory (Verify needs the whole content).
func TestCheckDoesNotInflateIntoMemory(t *testing.T) {
	content := make([]byte, 16<<20)
	id, stored := EncodeObject(KindBlob, content)
	content = nil
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	kind, err := Check(id, stored)
	runtime.ReadMemStats(&after)
	if err != nil || kind != KindBlob {
		t.Fatalf("Check: %v %v", kind, err)
	}
	if n := after.TotalAlloc - before.TotalAlloc; n > 1<<20 {
		t.Fatalf("Check of a %d-byte payload allocated %d bytes", len(stored), n)
	}
	stored[len(stored)-1] ^= 1
	if _, err := Check(id, stored); err == nil {
		t.Fatal("corruption not detected")
	}
}

// TestWriteContent: content is written as Decode returns it, compressed or
// not, and a payload that inflates to 16 MiB costs no memory for it.
func TestWriteContent(t *testing.T) {
	for _, content := range [][]byte{nil, []byte("x"), bytes.Repeat([]byte("abc\n"), 1000)} {
		for _, stored := range [][]byte{Encode(KindBlob, content), append([]byte{byte(KindBlob), codecRaw}, content...)} {
			var buf bytes.Buffer
			n, err := WriteContent(&buf, stored)
			if err != nil || n != int64(len(content)) || !bytes.Equal(buf.Bytes(), content) {
				t.Fatalf("codec %d: wrote %d bytes, %v", stored[1], n, err)
			}
		}
	}
	stored := Encode(KindBlob, make([]byte, 16<<20))
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	n, err := WriteContent(io.Discard, stored)
	runtime.ReadMemStats(&after)
	if err != nil || n != 16<<20 {
		t.Fatalf("wrote %d bytes, %v", n, err)
	}
	if a := after.TotalAlloc - before.TotalAlloc; a > 1<<20 {
		t.Fatalf("WriteContent of a %d-byte payload allocated %d bytes", len(stored), a)
	}
	if _, err := WriteContent(io.Discard, []byte{byte(KindBlob)}); err == nil {
		t.Fatal("an object too short was written")
	}
}

func BenchmarkDecode(b *testing.B) {
	stored := Encode(KindBlob, sourceBlob)
	b.SetBytes(int64(len(sourceBlob)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, _, err := Decode(stored); err != nil {
			b.Fatal(err)
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

func TestTreeEntriesSortedAndUnique(t *testing.T) {
	id := Hash(KindBlob, []byte("x"))
	for _, names := range [][]string{{"b", "a"}, {"a", "a"}, {"a", "c", "b"}, {"a.txt", "a"}} {
		var es []Entry
		for _, n := range names {
			es = append(es, Entry{Name: n, Mode: ModeFile, ID: id})
		}
		if _, err := DecodeTree((&Tree{Entries: es}).Encode()); err == nil {
			t.Errorf("tree %q accepted", names)
		}
		if _, err := DecodeUnode((&Unode{Dir: true, Entries: es}).Encode()); err == nil {
			t.Errorf("unode %q accepted", names)
		}
	}
	// A symlink and a directory with the same name: a checkout would make
	// the directory, replace it with the link, then write through the link.
	dup := &Tree{Entries: []Entry{{Name: "a", Mode: ModeSymlink, ID: id}, {Name: "a", Mode: ModeDir, ID: EmptyTreeID}}}
	if _, err := DecodeTree(dup.Encode()); err == nil {
		t.Error("tree with a repeated name accepted")
	}
	// SortEntries order (plain bytes) decodes.
	es := []Entry{{Name: "b", ID: id}, {Name: "a.txt", ID: id}, {Name: "a", ID: id}, {Name: "B", ID: id}, {Name: "a-b", ID: id}}
	SortEntries(es)
	if got, err := DecodeTree((&Tree{Entries: es}).Encode()); err != nil || len(got.Entries) != len(es) {
		t.Fatalf("sorted tree rejected: %v", err)
	}
}

// TestUnodeParentCountOverflow: a parent count whose size in bytes wraps
// around 64 bits must be rejected, not passed on to a slice expression.
func TestUnodeParentCountOverflow(t *testing.T) {
	for _, n := range []uint64{1 << 59, 1<<59 + 1, 1<<64 - 1, 3} {
		b := make([]byte, 1+IDSize)
		b = binary.AppendUvarint(b, n)
		b = append(b, make([]byte, 40)...)
		if _, err := DecodeUnode(b); err == nil {
			t.Fatalf("parent count %d accepted with 40 bytes left", n)
		}
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

// TestCheckAroundInMemoryLimit: content just under and just over the size
// Check hashes in memory verifies either way, and a wrong ID fails either way.
func TestCheckAroundInMemoryLimit(t *testing.T) {
	for _, n := range []int{checkInMemory - 1, checkInMemory, checkInMemory + 1} {
		content := bytes.Repeat([]byte{'k'}, n)
		content[n/2] = 'z'
		id, stored := EncodeObject(KindBlob, content)
		if kind, err := Check(id, stored); err != nil || kind != KindBlob {
			t.Fatalf("%d bytes: %v %v", n, kind, err)
		}
		if _, err := Check(Hash(KindBlob, content[1:]), stored); err == nil {
			t.Fatalf("%d bytes: wrong ID accepted", n)
		}
	}
}

func BenchmarkCheck(b *testing.B) {
	id, stored := EncodeObject(KindBlob, sourceBlob)
	b.SetBytes(int64(len(sourceBlob)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := Check(id, stored); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkVerify(b *testing.B) {
	id, stored := EncodeObject(KindBlob, sourceBlob)
	b.SetBytes(int64(len(sourceBlob)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, _, err := Verify(id, stored); err != nil {
			b.Fatal(err)
		}
	}
}

func TestValidGitlink(t *testing.T) {
	sha1, sha256 := strings.Repeat("0a", 20), strings.Repeat("f9", 32)
	for content, want := range map[string]bool{
		sha1: true, sha256: true,
		sha1 + "\n": false, " " + sha1: false, strings.ToUpper(sha1): false,
		sha1[:39]: false, sha1 + "0": false, strings.Repeat("g", 40): false,
		sha1 + "\nM 100644 inline x": false, "": false,
	} {
		if got := ValidGitlink([]byte(content)); got != want {
			t.Errorf("ValidGitlink(%q) = %v, want %v", content, got, want)
		}
	}
}

// zlibPadded compresses content into a zlib stream that starts with n empty
// stored blocks: valid zlib that inflates to content, but larger than what
// Encode writes.
func zlibPadded(t *testing.T, content []byte, n int) []byte {
	t.Helper()
	b := []byte{0x78, 0x01}
	for range n {
		b = append(b, 0x00, 0x00, 0x00, 0xff, 0xff) // not final, stored, length 0
	}
	var deflated bytes.Buffer
	fw, _ := flate.NewWriter(&deflated, flate.BestSpeed)
	fw.Write(content)
	fw.Close()
	b = append(b, deflated.Bytes()...)
	return binary.BigEndian.AppendUint32(b, adler32.Checksum(content))
}

// TestPaddedStoredForms: an object that hashes right but carries more bytes
// than Encode writes, after the zlib stream or inside it, is refused, so a
// client cannot make every tier store and serve a bloated copy.
func TestPaddedStoredForms(t *testing.T) {
	for _, n := range []int{1000, checkInMemory + 1000} {
		content := bytes.Repeat([]byte{'k'}, n)
		id, stored := EncodeObject(KindBlob, content)
		if stored[1] != codecZlib {
			t.Fatalf("%d bytes: not compressed", n)
		}
		trailing := append(bytes.Clone(stored), make([]byte, 4096)...)
		padded := append([]byte{byte(KindBlob), codecZlib}, zlibPadded(t, content, n/5+1)...)
		for name, bad := range map[string][]byte{"trailing bytes": trailing, "empty blocks": padded} {
			_, _, verr := Verify(id, bad)
			_, cerr := Check(id, bad)
			if verr == nil || cerr == nil {
				t.Errorf("%d bytes with %s (%d stored): accepted: %v %v", n, name, len(bad), verr, cerr)
			}
		}
		if _, _, err := Decode(trailing); err == nil {
			t.Errorf("%d bytes with trailing bytes: decoded", n)
		}
		// A stream padded less is still smaller than the raw form, which
		// any client could send anyway, so it is not worth refusing.
		light := append([]byte{byte(KindBlob), codecZlib}, zlibPadded(t, content, 1)...)
		if _, _, err := Verify(id, light); err != nil {
			t.Errorf("%d bytes, one empty block: %v", n, err)
		}
		if _, err := Check(id, light); err != nil {
			t.Errorf("%d bytes, one empty block: %v", n, err)
		}
	}
}
