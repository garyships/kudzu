package object

import (
	"bytes"
	"encoding/binary"
	"reflect"
	"testing"
)

// The decoders below read bytes from other tiers, which may be hostile:
// whatever they get, they must return an error rather than panic, and what
// they accept must survive a round trip through the encoder.

func FuzzDecodeTree(f *testing.F) {
	f.Add([]byte{})
	f.Add((&Tree{Entries: []Entry{
		{Name: "a.txt", Mode: ModeFile, ID: Hash(KindBlob, []byte("a"))},
		{Name: "bin", Mode: ModeDir, ID: EmptyTreeID},
	}}).Encode())
	f.Add([]byte("100644 a\x00short"))
	f.Add([]byte("0100644 ../x\x00" + string(make([]byte, IDSize))))
	f.Fuzz(func(t *testing.T, b []byte) {
		tr, err := DecodeTree(b)
		if err != nil {
			return
		}
		for _, e := range tr.Entries {
			if !validName(e.Name) {
				t.Fatalf("invalid name %q accepted", e.Name)
			}
		}
		again, err := DecodeTree(tr.Encode())
		if err != nil {
			t.Fatalf("re-encoded tree does not decode: %v", err)
		}
		if !reflect.DeepEqual(again, tr) {
			t.Fatalf("round trip changed the tree: %+v != %+v", again, tr)
		}
	})
}

func FuzzDecodeUnode(f *testing.F) {
	link := Hash(KindChangeset, []byte("c"))
	f.Add((&Unode{Linknode: link, Parents: []ID{Hash(KindUnode, []byte("p"))}, Mode: ModeFile, Content: Hash(KindBlob, []byte("x"))}).Encode())
	f.Add((&Unode{Dir: true, Linknode: link, Entries: []Entry{{Name: "f", Mode: ModeFile, ID: Hash(KindUnode, []byte("f"))}}}).Encode())
	f.Add(binary.AppendUvarint(make([]byte, 1+IDSize), 1<<59))
	f.Add([]byte{1})
	f.Fuzz(func(t *testing.T, b []byte) {
		u, err := DecodeUnode(b)
		if err != nil {
			return
		}
		again, err := DecodeUnode(u.Encode())
		if err != nil {
			t.Fatalf("re-encoded unode does not decode: %v", err)
		}
		if !reflect.DeepEqual(again, u) {
			t.Fatalf("round trip changed the unode: %+v != %+v", again, u)
		}
	})
}

func FuzzDecode(f *testing.F) {
	f.Add([]byte{})
	f.Add(Encode(KindBlob, []byte("hello")))
	f.Add(Encode(KindBlob, bytes.Repeat([]byte("compressible "), 100)))
	f.Add(Encode(KindTree, nil))
	f.Add([]byte{byte(KindBlob), codecZlib, 0x78, 0x9c})
	f.Add([]byte{byte(KindBlob), 7})
	f.Fuzz(func(t *testing.T, stored []byte) {
		kind, content, err := Decode(stored)
		if err != nil {
			return
		}
		id := Hash(kind, content)
		k, c, err := Verify(id, stored)
		if err != nil || k != kind || !bytes.Equal(c, content) {
			t.Fatalf("Verify disagrees with Decode: %v", err)
		}
		k, c, err = Decode(Encode(kind, content))
		if err != nil || k != kind || !bytes.Equal(c, content) {
			t.Fatalf("re-encoded object does not round trip: %v", err)
		}
		other := id
		other[0] ^= 1
		if _, _, err := Verify(other, stored); err == nil {
			t.Fatal("Verify accepted the wrong id")
		}
	})
}
