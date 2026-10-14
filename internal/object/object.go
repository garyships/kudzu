// Package object defines Kudzu's content-addressed objects (blobs, trees,
// unodes) and changesets, plus the stored form every tier keeps on disk, in
// memory and on the wire.
package object

import (
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
)

const IDSize = 32

// ID is the SHA-256 of an object's kind, length and content.
type ID [IDSize]byte

var Zero ID

func (id ID) IsZero() bool   { return id == Zero }
func (id ID) String() string { return hex.EncodeToString(id[:]) }
func (id ID) Short() string  { return id.String()[:12] }

func ParseID(s string) (ID, error) {
	var id ID
	if len(s) != 2*IDSize {
		return id, fmt.Errorf("invalid object id %q", s)
	}
	if _, err := hex.Decode(id[:], []byte(s)); err != nil {
		return id, fmt.Errorf("invalid object id %q", s)
	}
	return id, nil
}

func (id ID) MarshalText() ([]byte, error) {
	if id.IsZero() {
		return []byte{}, nil
	}
	return []byte(id.String()), nil
}

func (id *ID) UnmarshalText(b []byte) error {
	if len(b) == 0 {
		*id = Zero
		return nil
	}
	v, err := ParseID(string(b))
	if err != nil {
		return err
	}
	*id = v
	return nil
}

type Kind byte

const (
	KindBlob      Kind = 'b'
	KindTree      Kind = 't'
	KindUnode     Kind = 'u'
	KindChangeset Kind = 'c'
)

func (k Kind) String() string {
	switch k {
	case KindBlob:
		return "blob"
	case KindTree:
		return "tree"
	case KindUnode:
		return "unode"
	case KindChangeset:
		return "changeset"
	}
	return "kind(" + strconv.Itoa(int(k)) + ")"
}

// Hash returns the ID of content of the given kind.
func Hash(kind Kind, content []byte) ID {
	h := sha256.New()
	h.Write([]byte(kind.String()))
	h.Write([]byte{' '})
	h.Write([]byte(strconv.Itoa(len(content))))
	h.Write([]byte{0})
	h.Write(content)
	var id ID
	h.Sum(id[:0])
	return id
}

// Stored form: [kind][codec][payload]. Every tier stores and serves exactly
// these bytes, so caches never decompress, recompress or delta-encode.
const (
	codecRaw  byte = 0
	codecZlib byte = 1

	compressThreshold = 256
)

var zwPool = sync.Pool{New: func() any {
	w, _ := zlib.NewWriterLevel(io.Discard, zlib.BestSpeed)
	return w
}}

func Encode(kind Kind, content []byte) []byte {
	// Trees and unodes are mostly 32-byte hashes, which don't compress;
	// only blobs are worth the CPU.
	if kind == KindBlob && len(content) >= compressThreshold {
		var buf bytes.Buffer
		buf.Grow(len(content)/2 + 16)
		buf.WriteByte(byte(kind))
		buf.WriteByte(codecZlib)
		zw := zwPool.Get().(*zlib.Writer)
		zw.Reset(&buf)
		zw.Write(content)
		zw.Close()
		zwPool.Put(zw)
		if buf.Len() < len(content)+2 {
			return buf.Bytes()
		}
	}
	out := make([]byte, 2+len(content))
	out[0] = byte(kind)
	out[1] = codecRaw
	copy(out[2:], content)
	return out
}

func Decode(stored []byte) (Kind, []byte, error) {
	if len(stored) < 2 {
		return 0, nil, errors.New("object too short")
	}
	kind := Kind(stored[0])
	switch stored[1] {
	case codecRaw:
		return kind, stored[2:], nil
	case codecZlib:
		zr, err := zlib.NewReader(bytes.NewReader(stored[2:]))
		if err != nil {
			return 0, nil, err
		}
		defer zr.Close()
		b, err := io.ReadAll(zr)
		return kind, b, err
	}
	return 0, nil, fmt.Errorf("unknown object codec %d", stored[1])
}

// Verify decodes a stored object and checks that it hashes to id.
func Verify(id ID, stored []byte) (Kind, []byte, error) {
	kind, content, err := Decode(stored)
	if err != nil {
		return 0, nil, err
	}
	if Hash(kind, content) != id {
		return 0, nil, fmt.Errorf("object %s: hash mismatch", id.Short())
	}
	return kind, content, nil
}

// EncodeObject is a convenience returning both the ID and stored form.
func EncodeObject(kind Kind, content []byte) (ID, []byte) {
	return Hash(kind, content), Encode(kind, content)
}
