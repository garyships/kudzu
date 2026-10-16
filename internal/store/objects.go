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
	"runtime"
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

// Toucher is an object store that records when an object was last vouched
// for. Garbage collection never removes an object touched within its grace
// period, so the server touches every existing object it tells a client it
// has, or reuses in a new changeset.
type Toucher interface {
	// Touch returns ErrNotFound if id is not stored.
	Touch(ctx context.Context, id object.ID) error
}

// Sweeper is an object store that garbage collection can enumerate and
// delete from.
type Sweeper interface {
	// Walk calls fn for every stored object with its size and the time it
	// was last written or touched.
	Walk(ctx context.Context, fn func(id object.ID, size int64, mtime time.Time) error) error
	// DeleteIfOlder removes id unless it was written or touched at or
	// after cutoff. It reports whether it removed the object and its size.
	DeleteIfOlder(ctx context.Context, id object.ID, cutoff time.Time) (bool, int64, error)
}

// Touch marks id as in use if objs supports it, and otherwise just checks
// that id exists. It returns ErrNotFound if id is not stored.
func Touch(ctx context.Context, objs Objects, id object.ID) error {
	if t, ok := objs.(Toucher); ok {
		return t.Touch(ctx, id)
	}
	ok, err := objs.Has(ctx, id)
	if err == nil && !ok {
		err = ErrNotFound
	}
	return err
}

// touchInterval is how stale an object's mtime may get before Touch
// rewrites it. Garbage collection is only safe alongside a live server when
// its grace period is well above this.
const touchInterval = 10 * time.Minute

// FS stores one file per object under dir/ab/cdef.... When maxBytes > 0 it
// acts as a bounded cache: least recently used objects are evicted.
//
// An empty file is not an object (every stored object has a two-byte
// header); it is what a crash can leave of a write that never reached the
// disk, so FS treats it as missing and the object gets written again.
type FS struct {
	dir      string
	maxBytes int64
	durable  bool // fsync objects and their directories (OpenDurableFS)
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

// OpenDurableFS opens an unbounded FS for an authoritative store, whose
// metadata commits changesets and refs that name its objects. Put syncs each
// object file before renaming it into place and its directory after, so an
// object is on disk before Put returns. Caches and working copies use
// OpenFS: an object lost there is just fetched again.
func OpenDurableFS(dir string) (*FS, error) {
	s, err := OpenFS(dir, 0)
	if err != nil {
		return nil, err
	}
	s.durable = true
	// Make the shard directories OpenFS may have just created durable too.
	if err := syncDir(dir); err != nil {
		return nil, err
	}
	if err := syncDir(filepath.Dir(dir)); err != nil {
		return nil, err
	}
	return s, nil
}

// syncDir makes the entries created or renamed in dir durable. Windows
// cannot sync a directory handle; there this is a no-op.
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}

func (s *FS) path(id object.ID) string {
	h := id.String()
	return filepath.Join(s.dir, h[:2], h[2:])
}

func (s *FS) Get(_ context.Context, id object.ID) ([]byte, error) {
	p := s.path(id)
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) || err == nil && len(b) == 0 {
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

func (s *FS) Touch(_ context.Context, id object.ID) error {
	p := s.path(id)
	info, err := os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) || err == nil && info.Size() == 0 {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if time.Since(info.ModTime()) > touchInterval {
		now := time.Now()
		if err := os.Chtimes(p, now, now); errors.Is(err, fs.ErrNotExist) {
			return ErrNotFound // removed in between
		}
	}
	return nil
}

func (s *FS) Walk(ctx context.Context, fn func(id object.ID, size int64, mtime time.Time) error) error {
	for i := 0; i < 256; i++ {
		shard := fmt.Sprintf("%02x", i)
		entries, err := os.ReadDir(filepath.Join(s.dir, shard))
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			id, err := object.ParseID(shard + e.Name())
			if err != nil {
				continue // not an object
			}
			info, err := e.Info()
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			if err := fn(id, info.Size(), info.ModTime()); err != nil {
				return err
			}
		}
	}
	return nil
}

// DeleteIfOlder re-reads the object's mtime right before removing it, so an
// object touched since Walk saw it survives.
func (s *FS) DeleteIfOlder(_ context.Context, id object.ID, cutoff time.Time) (bool, int64, error) {
	p := s.path(id)
	info, err := os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	if !info.ModTime().Before(cutoff) {
		return false, 0, nil
	}
	if err := os.Remove(p); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, 0, nil
		}
		return false, 0, err
	}
	if s.maxBytes > 0 {
		s.size.Add(-info.Size())
	}
	return true, info.Size(), nil
}

// CleanTmp removes temporary files older than cutoff that a crashed Put
// left behind.
func (s *FS) CleanTmp(cutoff time.Time) (int, error) {
	dir := filepath.Join(s.dir, "tmp")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
			if os.Remove(filepath.Join(dir, e.Name())) == nil {
				n++
			}
		}
	}
	return n, nil
}

func (s *FS) Has(_ context.Context, id object.ID) (bool, error) {
	info, err := os.Stat(s.path(id))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil && info.Size() > 0, err
}

// Put writes via a temp file and rename. Objects are immutable, so racing
// writers of the same ID are harmless: both renames install identical bytes.
// A durable FS syncs the file before the rename and the directory after.
func (s *FS) Put(_ context.Context, id object.ID, stored []byte) error {
	p := s.path(id)
	if s.maxBytes > 0 {
		if info, err := os.Stat(p); err == nil && info.Size() > 0 {
			return nil // keep size accounting exact for bounded caches
		}
	}
	f, err := os.CreateTemp(filepath.Join(s.dir, "tmp"), "obj-")
	if err != nil {
		return err
	}
	_, err = f.Write(stored)
	if err == nil && s.durable {
		err = f.Sync()
	}
	if err != nil {
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
	if s.durable {
		if err := syncDir(filepath.Dir(p)); err != nil {
			return err
		}
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

// Remove drops id from the cache.
func (s *Mem) Remove(id object.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.m[id]; ok {
		s.ll.Remove(e)
		delete(s.m, id)
		s.size -= int64(len(e.Value.(*memEntry).data))
	}
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

// Touch goes to the backing store: the memory layer may still hold an
// object that garbage collection has removed from it.
func (c *Cached) Touch(ctx context.Context, id object.ID) error {
	err := Touch(ctx, c.Backing, id)
	if errors.Is(err, ErrNotFound) {
		c.Mem.Remove(id)
	}
	return err
}

func (c *Cached) Walk(ctx context.Context, fn func(id object.ID, size int64, mtime time.Time) error) error {
	sw, ok := c.Backing.(Sweeper)
	if !ok {
		return fmt.Errorf("object store %T cannot be garbage collected", c.Backing)
	}
	return sw.Walk(ctx, fn)
}

func (c *Cached) DeleteIfOlder(ctx context.Context, id object.ID, cutoff time.Time) (bool, int64, error) {
	sw, ok := c.Backing.(Sweeper)
	if !ok {
		return false, 0, fmt.Errorf("object store %T cannot be garbage collected", c.Backing)
	}
	removed, size, err := sw.DeleteIfOlder(ctx, id, cutoff)
	if removed {
		c.Mem.Remove(id)
	}
	return removed, size, err
}
