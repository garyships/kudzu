// Package store holds Kudzu's two storage roles: Meta (the authoritative
// changeset log, refs and derived data; Spanner in the article, SQLite here)
// and Objects (immutable content-addressed objects; cloud storage in the
// article, a directory or memory here).
package store

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/garyships/kudzu/internal/object"
)

var (
	ErrNotFound  = errors.New("not found")
	ErrConflict  = errors.New("compare-and-swap conflict")
	ErrAmbiguous = errors.New("ambiguous id prefix")
)

// Objects stores objects in their stored form (see object.Encode). Objects
// never change once written, so any copy anywhere is as good as any other.
type Objects interface {
	Get(ctx context.Context, id object.ID) ([]byte, error)
	Has(ctx context.Context, id object.ID) (bool, error)
	Put(ctx context.Context, id object.ID, stored []byte) error
}

// FS stores one file per object under dir/ab/cdef.... When maxBytes > 0 it
// acts as a bounded cache: least recently used objects are evicted.
type FS struct {
	dir      string
	maxBytes int64
	size     atomic.Int64
	gcMu     sync.Mutex
}

func OpenFS(dir string, maxBytes int64) (*FS, error) {
	if err := os.MkdirAll(filepath.Join(dir, "tmp"), 0o755); err != nil {
		return nil, err
	}
	// Create all 256 shard directories up front so Put never has to.
	for i := 0; i < 256; i++ {
		if err := os.MkdirAll(filepath.Join(dir, fmt.Sprintf("%02x", i)), 0o755); err != nil {
			return nil, err
		}
	}
	s := &FS{dir: dir, maxBytes: maxBytes}
	if maxBytes > 0 {
		var total int64
		filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				if info, err := d.Info(); err == nil {
					total += info.Size()
				}
			}
			return nil
		})
		s.size.Store(total)
	}
	return s, nil
}

func (s *FS) path(id object.ID) string {
	h := id.String()
	return filepath.Join(s.dir, h[:2], h[2:])
}

func (s *FS) Get(_ context.Context, id object.ID) ([]byte, error) {
	p := s.path(id)
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err == nil && s.maxBytes > 0 {
		// Approximate LRU: bump mtime at most once a minute per object.
		if info, err := os.Stat(p); err == nil && time.Since(info.ModTime()) > time.Minute {
			now := time.Now()
			os.Chtimes(p, now, now)
		}
	}
	return b, err
}

func (s *FS) Has(_ context.Context, id object.ID) (bool, error) {
	_, err := os.Stat(s.path(id))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// Put writes via a temp file and rename. Objects are immutable, so racing
// writers of the same ID are harmless: both renames install identical bytes.
func (s *FS) Put(_ context.Context, id object.ID, stored []byte) error {
	p := s.path(id)
	if s.maxBytes > 0 {
		if _, err := os.Stat(p); err == nil {
			return nil // keep size accounting exact for bounded caches
		}
	}
	f, err := os.CreateTemp(filepath.Join(s.dir, "tmp"), "obj-")
	if err != nil {
		return err
	}
	if _, err := f.Write(stored); err != nil {
		f.Close()
		os.Remove(f.Name())
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return err
	}
	if err := os.Rename(f.Name(), p); err != nil {
		os.Remove(f.Name())
		return err
	}
	if s.maxBytes > 0 && s.size.Add(int64(len(stored))) > s.maxBytes {
		go s.GC(s.maxBytes * 9 / 10)
	}
	return nil
}

// GC evicts least recently used objects until the store is under target bytes.
func (s *FS) GC(target int64) (removed int, err error) {
	if !s.gcMu.TryLock() {
		return 0, nil
	}
	defer s.gcMu.Unlock()
	type file struct {
		path  string
		size  int64
		mtime time.Time
	}
	var files []file
	var total int64
	err = filepath.WalkDir(s.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Base(filepath.Dir(p)) == "tmp" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		files = append(files, file{p, info.Size(), info.ModTime()})
		total += info.Size()
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].mtime.Before(files[j].mtime) })
	for _, f := range files {
		if total <= target {
			break
		}
		if os.Remove(f.path) == nil {
			total -= f.size
			removed++
		}
	}
	s.size.Store(total)
	return removed, err
}

func (s *FS) Size() int64 { return s.size.Load() }

// Mem is an in-memory LRU bounded by total bytes.
type Mem struct {
	mu   sync.Mutex
	max  int64
	size int64
	ll   *list.List
	m    map[object.ID]*list.Element
}

type memEntry struct {
	id   object.ID
	data []byte
}

func NewMem(maxBytes int64) *Mem {
	return &Mem{max: maxBytes, ll: list.New(), m: make(map[object.ID]*list.Element)}
}

func (s *Mem) Get(_ context.Context, id object.ID) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.m[id]; ok {
		s.ll.MoveToFront(e)
		return e.Value.(*memEntry).data, nil
	}
	return nil, ErrNotFound
}

func (s *Mem) Has(_ context.Context, id object.ID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.m[id]
	return ok, nil
}

func (s *Mem) Put(_ context.Context, id object.ID, stored []byte) error {
	if int64(len(stored)) > s.max {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.m[id]; ok {
		s.ll.MoveToFront(e)
		return nil
	}
	s.m[id] = s.ll.PushFront(&memEntry{id, stored})
	s.size += int64(len(stored))
	for s.size > s.max {
		old := s.ll.Back()
		s.ll.Remove(old)
		me := old.Value.(*memEntry)
		delete(s.m, me.id)
		s.size -= int64(len(me.data))
	}
	return nil
}

func (s *Mem) Stats() (objects int, bytes int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m), s.size
}

// Cached layers a memory LRU in front of a backing store.
type Cached struct {
	Mem     *Mem
	Backing Objects
}

func (c *Cached) Get(ctx context.Context, id object.ID) ([]byte, error) {
	if b, err := c.Mem.Get(ctx, id); err == nil {
		return b, nil
	}
	b, err := c.Backing.Get(ctx, id)
	if err == nil {
		c.Mem.Put(ctx, id, b)
	}
	return b, err
}

func (c *Cached) Has(ctx context.Context, id object.ID) (bool, error) {
	if ok, _ := c.Mem.Has(ctx, id); ok {
		return true, nil
	}
	return c.Backing.Has(ctx, id)
}

func (c *Cached) Put(ctx context.Context, id object.ID, stored []byte) error {
	if err := c.Backing.Put(ctx, id, stored); err != nil {
		return err
	}
	return c.Mem.Put(ctx, id, stored)
}
