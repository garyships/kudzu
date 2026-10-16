// Package proto is Kudzu's object protocol. Every tier (server, regional
// cache, per-node daemon) answers the same RPCs, so a client never needs to
// know the cache topology.
//
// Negotiation is want-based: the client never tells the server what it has.
// It asks for specific objects by ID, starting from a root tree, and since
// every object is immutable any cache can answer any request.
//
//	GET  /objects/{id}     one object in stored form
//	POST /objects/fetch    body: concatenated 32-byte IDs; response: frames
//
// A frame is: id (32 bytes) | status (1 byte: 0 ok, 1 missing) | length
// (uint32, big endian) | stored object. Responses stream object by object,
// so an interrupted transfer resumes by re-asking for what didn't arrive.
package proto

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"

	"github.com/garyships/kudzu/internal/object"
)

const (
	statusOK      = 0
	statusMissing = 1

	// MaxFetchIDs bounds one fetch request.
	MaxFetchIDs = 1 << 16
)

func WriteFrame(w io.Writer, id object.ID, stored []byte) error {
	var hdr [object.IDSize + 5]byte
	copy(hdr[:], id[:])
	hdr[object.IDSize] = statusOK
	binary.BigEndian.PutUint32(hdr[object.IDSize+1:], uint32(len(stored)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(stored)
	return err
}

func WriteMissing(w io.Writer, id object.ID) error {
	var hdr [object.IDSize + 5]byte
	copy(hdr[:], id[:])
	hdr[object.IDSize] = statusMissing
	_, err := w.Write(hdr[:])
	return err
}

type FrameReader struct {
	r *bufio.Reader
}

func NewFrameReader(r io.Reader) *FrameReader {
	return &FrameReader{r: bufio.NewReaderSize(r, 1<<16)}
}

// Next returns the next frame. data is freshly allocated and safe to keep.
// It returns io.EOF at a clean end of stream.
func (f *FrameReader) Next() (id object.ID, data []byte, missing bool, err error) {
	var hdr [object.IDSize + 5]byte
	if _, err = io.ReadFull(f.r, hdr[:]); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			err = fmt.Errorf("truncated frame: %w", err)
		}
		return
	}
	copy(id[:], hdr[:object.IDSize])
	switch hdr[object.IDSize] {
	case statusMissing:
		return id, nil, true, nil
	case statusOK:
	default:
		return id, nil, false, fmt.Errorf("frame for object %s: unknown status %d", id.Short(), hdr[object.IDSize])
	}
	n := binary.BigEndian.Uint32(hdr[object.IDSize+1:])
	if n > object.MaxStoredSize {
		return id, nil, false, fmt.Errorf("frame for object %s: %d bytes is over the %d-byte object limit", id.Short(), n, object.MaxStoredSize)
	}
	if data, err = readFrameData(f.r, int(n)); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return id, nil, false, fmt.Errorf("truncated frame for object %s: %w", id.Short(), err)
	}
	return id, data, false, nil
}

// readFrameData reads an n-byte frame payload. A header costs its sender
// 37 bytes, so the buffer grows with the data that actually arrives rather
// than being allocated up front at the length the header claims.
func readFrameData(r io.Reader, n int) ([]byte, error) {
	b := make([]byte, 0, min(n, 1<<20))
	for len(b) < n {
		if len(b) == cap(b) {
			b = append(make([]byte, 0, len(b)+min(len(b), n-len(b))), b...)
		}
		k, err := io.ReadFull(r, b[len(b):cap(b)])
		b = b[:len(b)+k]
		if err != nil {
			return nil, err
		}
	}
	return b, nil
}

// EncodeIDs and DecodeIDs handle fetch request bodies.
func EncodeIDs(ids []object.ID) []byte {
	b := make([]byte, 0, len(ids)*object.IDSize)
	for _, id := range ids {
		b = append(b, id[:]...)
	}
	return b
}

func DecodeIDs(b []byte) ([]object.ID, error) {
	if len(b)%object.IDSize != 0 {
		return nil, errors.New("request body is not a list of object ids")
	}
	ids := make([]object.ID, len(b)/object.IDSize)
	for i := range ids {
		copy(ids[i][:], b[i*object.IDSize:])
	}
	return ids, nil
}

// Source answers object requests; servers read their stores, caches consult
// their upstream on a miss.
type Source interface {
	FetchMany(ctx context.Context, ids []object.ID, emit func(id object.ID, stored []byte) error) (missing []object.ID, err error)
}

// Counters are shared request statistics every tier reports at /stats.
type Counters struct {
	ObjectsServed atomic.Int64
	BytesServed   atomic.Int64
	Requests      atomic.Int64
}

// RegisterObjectRoutes serves the read side of the object protocol from src.
func RegisterObjectRoutes(mux *http.ServeMux, src Source, c *Counters) {
	mux.HandleFunc("GET /objects/{id}", func(w http.ResponseWriter, req *http.Request) {
		c.Requests.Add(1)
		id, err := object.ParseID(req.PathValue("id"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var data []byte
		missing, err := src.FetchMany(req.Context(), []object.ID{id}, func(_ object.ID, b []byte) error {
			data = b
			return nil
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if len(missing) > 0 {
			http.Error(w, "object not found", http.StatusNotFound)
			return
		}
		c.ObjectsServed.Add(1)
		c.BytesServed.Add(int64(len(data)))
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Write(data)
	})
	mux.HandleFunc("POST /objects/fetch", func(w http.ResponseWriter, req *http.Request) {
		c.Requests.Add(1)
		body, err := io.ReadAll(io.LimitReader(req.Body, MaxFetchIDs*object.IDSize+1))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		ids, err := DecodeIDs(body)
		if err != nil || len(ids) > MaxFetchIDs {
			http.Error(w, "bad fetch request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/x-kudzu-frames")
		bw := bufio.NewWriterSize(w, 1<<16)
		missing, err := src.FetchMany(req.Context(), ids, func(id object.ID, b []byte) error {
			c.ObjectsServed.Add(1)
			c.BytesServed.Add(int64(len(b)))
			return WriteFrame(bw, id, b)
		})
		if err != nil {
			// Frames already sent stay valid; the client re-asks for the rest.
			bw.Flush()
			return
		}
		for _, id := range missing {
			WriteMissing(bw, id)
		}
		bw.Flush()
	})
}
