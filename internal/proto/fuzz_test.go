package proto

import (
	"bytes"
	"io"
	"testing"

	"github.com/garyships/kudzu/internal/object"
)

// FuzzFrameReader: whatever a peer sends, the reader returns frames or an
// error without panicking, never returns more data than it was sent, and
// the frames it returns read back the same after being written again.
func FuzzFrameReader(f *testing.F) {
	id, stored := object.EncodeObject(object.KindBlob, []byte("hello"))
	var ok, miss bytes.Buffer
	WriteFrame(&ok, id, stored)
	WriteMissing(&miss, id)
	f.Add([]byte{})
	f.Add(ok.Bytes())
	f.Add(append(ok.Bytes(), miss.Bytes()...))
	f.Add(ok.Bytes()[:ok.Len()-1])
	f.Add(header(id, statusOK, 1<<30))
	f.Add(header(id, statusOK, 1<<32-1))
	f.Add(header(id, 2, 0))
	f.Fuzz(func(t *testing.T, in []byte) {
		type frame struct {
			id   object.ID
			data []byte
			miss bool
		}
		var frames []frame
		var out bytes.Buffer
		fr := NewFrameReader(bytes.NewReader(in))
		for {
			id, data, miss, err := fr.Next()
			if err != nil {
				break
			}
			if len(data) > len(in) {
				t.Fatalf("frame of %d bytes from %d bytes of input", len(data), len(in))
			}
			frames = append(frames, frame{id, data, miss})
			if miss {
				WriteMissing(&out, id)
			} else {
				WriteFrame(&out, id, data)
			}
		}
		fr = NewFrameReader(&out)
		for _, want := range frames {
			id, data, miss, err := fr.Next()
			if err != nil || id != want.id || miss != want.miss || !bytes.Equal(data, want.data) {
				t.Fatalf("frame %s did not round trip: %v", want.id.Short(), err)
			}
		}
		if _, _, _, err := fr.Next(); err != io.EOF {
			t.Fatalf("re-encoded stream does not end cleanly: %v", err)
		}
	})
}
