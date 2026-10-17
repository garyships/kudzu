package proto

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"

	"github.com/garyships/kudzu/internal/object"
)

func header(id object.ID, status byte, n uint32) []byte {
	var hdr [object.IDSize + 5]byte
	copy(hdr[:], id[:])
	hdr[object.IDSize] = status
	binary.BigEndian.PutUint32(hdr[object.IDSize+1:], n)
	return hdr[:]
}

func TestFrameRoundTrip(t *testing.T) {
	a, sa := object.EncodeObject(object.KindBlob, []byte("hello"))
	b, sb := object.EncodeObject(object.KindBlob, bytes.Repeat([]byte("big "), 1<<19))
	gone := object.Hash(object.KindBlob, []byte("gone"))
	var buf bytes.Buffer
	WriteFrame(&buf, a, sa)
	WriteMissing(&buf, gone)
	WriteFrame(&buf, b, sb)
	fr := NewFrameReader(&buf)
	for _, want := range []struct {
		id   object.ID
		data []byte
		miss bool
	}{{a, sa, false}, {gone, nil, true}, {b, sb, false}} {
		id, data, miss, err := fr.Next()
		if err != nil || id != want.id || miss != want.miss || !bytes.Equal(data, want.data) {
			t.Fatalf("frame %s: id %s miss %v err %v", want.id.Short(), id.Short(), miss, err)
		}
	}
	if _, _, _, err := fr.Next(); err != io.EOF {
		t.Fatalf("end of stream: %v", err)
	}
}

// TestFrameReaderClaimedLength: a 37-byte header claiming a gigabyte costs
// the reader what actually arrives, not what the header claims.
func TestFrameReaderClaimedLength(t *testing.T) {
	var id object.ID
	in := append(header(id, statusOK, 1<<30), "only this"...)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, _, _, err := NewFrameReader(bytes.NewReader(in)).Next()
	runtime.ReadMemStats(&after)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated frame: %v", err)
	}
	if n := after.TotalAlloc - before.TotalAlloc; n > 8<<20 {
		t.Fatalf("reading a frame header that claims 1 GiB allocated %d MiB", n>>20)
	}
}

// TestFrameReaderLimits: frames longer than any stored object, and frames
// with an unknown status, are rejected before their payload is read.
func TestFrameReaderLimits(t *testing.T) {
	var id object.ID
	for _, in := range [][]byte{
		header(id, statusOK, object.MaxStoredSize+1),
		header(id, statusOK, 1<<32-1),
		header(id, 7, 0),
	} {
		_, _, _, err := NewFrameReader(bytes.NewReader(in)).Next()
		if err == nil || errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("header %x: %v", in[object.IDSize:], err)
		}
	}
	// The largest stored object still has a valid length.
	_, _, _, err := NewFrameReader(bytes.NewReader(header(id, statusOK, object.MaxStoredSize))).Next()
	if !errors.Is(err, io.ErrUnexpectedEOF) || strings.Contains(err.Error(), "limit") {
		t.Fatalf("frame of MaxStoredSize: %v", err)
	}
}
