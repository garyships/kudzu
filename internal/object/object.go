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
	writeHeader(h, kind, len(content))
	h.Write(content)
	var id ID
	h.Sum(id[:0])
	return id
}

// writeHeader writes what an object's hash covers before its content.
func writeHeader(h io.Writer, kind Kind, n int) {
	h.Write([]byte(kind.String()))
	h.Write([]byte{' '})
	h.Write([]byte(strconv.Itoa(n)))
	h.Write([]byte{0})
}

// MaxSize bounds the content of one object (1 GiB). Decoding stops there,
// so that neither a frame header claiming a length nor a small payload that
// inflates without end can make a tier allocate more than that.
const MaxSize = 1 << 30

// MaxStoredSize bounds the stored form of one object: Encode compresses
// only when that is smaller, so it adds at most the two header bytes.
const MaxStoredSize = MaxSize + 2

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

// Decode returns the kind and content of a stored object. Content larger
// than MaxSize is an error.
func Decode(stored []byte) (Kind, []byte, error) {
	return decode(stored, MaxSize)
}

func decode(stored []byte, limit int) (Kind, []byte, error) {
	if len(stored) < 2 {
		return 0, nil, errors.New("object too short")
	}
	kind := Kind(stored[0])
	switch stored[1] {
	case codecRaw:
		if len(stored)-2 > limit {
			return 0, nil, tooLarge(limit)
		}
		return kind, stored[2:], nil
	case codecZlib:
		b, err := inflate(stored[2:], limit)
		if err != nil {
			return 0, nil, err
		}
		return kind, b, nil
	}
	return 0, nil, fmt.Errorf("unknown object codec %d", stored[1])
}

func tooLarge(limit int) error {
	return fmt.Errorf("object content is larger than %d bytes", limit)
}

// An inflater is a zlib reader kept with the bytes.Reader it reads from.
// Decode runs for every object a clone verifies and every blob blame, diff
// or cat reads; a fresh zlib reader allocates tens of kilobytes, so they
// are pooled like the writers.
type inflater struct {
	br bytes.Reader
	zr io.ReadCloser
}

var inflaters sync.Pool

func getInflater(payload []byte) (*inflater, error) {
	f, _ := inflaters.Get().(*inflater)
	if f == nil {
		f = new(inflater)
	}
	f.br.Reset(payload)
	var err error
	if f.zr == nil {
		f.zr, err = zlib.NewReader(&f.br)
	} else {
		err = f.zr.(zlib.Resetter).Reset(&f.br, nil)
	}
	if err != nil {
		putInflater(f)
		return nil, err
	}
	return f, nil
}

func putInflater(f *inflater) {
	f.br.Reset(nil) // don't keep the payload alive from the pool
	inflaters.Put(f)
}

// inflate decompresses a zlib payload of at most limit bytes. Blobs
// typically shrink 3-4x, so the buffer starts at that guess (capped, so
// that a large blob that barely compressed does not carry much slack) and
// doubles from there, never past the limit.
func inflate(payload []byte, limit int) ([]byte, error) {
	f, err := getInflater(payload)
	if err != nil {
		return nil, err
	}
	defer putInflater(f)
	lr := io.LimitedReader{R: f.zr, N: int64(limit) + 1}
	b := make([]byte, 0, min(4*min(len(payload), 1<<18), limit)+bytes.MinRead)
	for {
		if len(b) == cap(b) {
			b = append(make([]byte, 0, len(b)+min(len(b), limit+1-len(b))), b...)
		}
		n, err := lr.Read(b[len(b):cap(b)])
		b = b[:len(b)+n]
		if len(b) > limit {
			return nil, tooLarge(limit)
		}
		if err == io.EOF {
			return b, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// inflateTo decompresses at most limit bytes of a zlib payload into w.
func inflateTo(w io.Writer, payload []byte, limit int64) (int64, error) {
	f, err := getInflater(payload)
	if err != nil {
		return 0, err
	}
	defer putInflater(f)
	return io.Copy(w, &io.LimitedReader{R: f.zr, N: limit})
}

// Verify decodes a stored object and checks that it hashes to id.
func Verify(id ID, stored []byte) (Kind, []byte, error) {
	return verify(id, stored, MaxSize)
}

func verify(id ID, stored []byte, limit int) (Kind, []byte, error) {
	kind, content, err := decode(stored, limit)
	if err != nil {
		return 0, nil, fmt.Errorf("object %s: %w", id.Short(), err)
	}
	if Hash(kind, content) != id {
		return 0, nil, fmt.Errorf("object %s: hash mismatch", id.Short())
	}
	return kind, content, nil
}

// Check is Verify for callers that only store or forward the object, such
// as the server taking an upload or a cache filling a miss. It never holds
// the content: a compressed payload is decompressed twice, once to count
// the bytes that the hash header needs and once into the hash, so a payload
// that inflates to MaxSize costs CPU but not memory.
func Check(id ID, stored []byte) (Kind, error) {
	return check(id, stored, MaxSize)
}

func check(id ID, stored []byte, limit int) (Kind, error) {
	if len(stored) < 2 || stored[1] != codecZlib {
		// Raw content is in memory already.
		kind, _, err := verify(id, stored, limit)
		return kind, err
	}
	kind := Kind(stored[0])
	n, err := inflateTo(io.Discard, stored[2:], int64(limit)+1)
	if err == nil && n > int64(limit) {
		err = tooLarge(limit)
	}
	if err != nil {
		return 0, fmt.Errorf("object %s: %w", id.Short(), err)
	}
	h := sha256.New()
	writeHeader(h, kind, int(n))
	if m, err := inflateTo(h, stored[2:], n); err != nil || m != n {
		return 0, fmt.Errorf("object %s: payload inflated differently twice", id.Short())
	}
	var got ID
	h.Sum(got[:0])
	if got != id {
		return 0, fmt.Errorf("object %s: hash mismatch", id.Short())
	}
	return kind, nil
}

// EncodeObject is a convenience returning both the ID and stored form.
func EncodeObject(kind Kind, content []byte) (ID, []byte) {
	return Hash(kind, content), Encode(kind, content)
}
