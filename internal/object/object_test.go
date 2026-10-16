package object

import (
	"bytes"
	"encoding/binary"
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
			_, got, err := decode(stored, limit)
			_, _, verr := verify(id, stored, limit)
			_, cerr := check(id, stored, limit)
			if n > limit {
				if err == nil || verr == nil || cerr == nil {
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
	for _, p := range []string{"a", "a/b.txt", ".github/x"} {
		if err := ValidatePath(p); err != nil {
			t.Fatalf("%q rejected: %v", p, err)
		}
	}
	for _, p := range []string{"", "/a", "a/", "a//b", "../x", "a/./b", ".kudzu/state"} {
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
