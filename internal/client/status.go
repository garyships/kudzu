package client

import (
	"bufio"
	"context"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/garyships/kudzu/internal/object"
)

// FileStatus codes: M modified, D deleted, ? untracked.
type FileStatus struct {
	Path string
	Code byte
	Mode uint32
}

type ignorer struct {
	patterns []string
}

func loadIgnore(root string) *ignorer {
	ig := &ignorer{}
	f, err := os.Open(filepath.Join(root, ".kudzuignore"))
	if err != nil {
		return ig
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l != "" && !strings.HasPrefix(l, "#") {
			ig.patterns = append(ig.patterns, l)
		}
	}
	return ig
}

// match supports "name" (any path component), "dir/" (directories only) and
// slash-containing globs matched against the full path.
func (ig *ignorer) match(p string, isDir bool) bool {
	base := path.Base(p)
	for _, pat := range ig.patterns {
		dirOnly := strings.HasSuffix(pat, "/")
		pat = strings.Trim(pat, "/")
		if dirOnly && !isDir {
			continue
		}
		if strings.Contains(pat, "/") {
			if ok, _ := path.Match(pat, p); ok {
				return true
			}
		} else if ok, _ := path.Match(pat, base); ok {
			return true
		}
	}
	return false
}

func modeOf(info fs.FileInfo) (uint32, bool) {
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		return object.ModeSymlink, true
	case info.Mode().IsRegular():
		if info.Mode().Perm()&0o111 != 0 {
			return object.ModeExec, true
		}
		return object.ModeFile, true
	}
	return 0, false
}

func readContent(full string, mode uint32) ([]byte, error) {
	if mode == object.ModeSymlink {
		target, err := os.Readlink(full)
		return []byte(target), err
	}
	return os.ReadFile(full)
}

// Status compares the working copy with the index. Files whose size and
// mtime match the index are trusted without being read.
func (w *WC) Status(ctx context.Context) ([]FileStatus, error) {
	sp := w.State.Sparse
	ig := loadIgnore(w.Root)
	seen := map[string]bool{}
	var out []FileStatus
	err := filepath.WalkDir(w.Root, func(full string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(w.Root, full)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			if rel == metaDir || !sp.WantDir(rel) || (ig.match(rel, true) && !w.tracksUnder(rel)) {
				return filepath.SkipDir
			}
			return nil
		}
		if !sp.WantFile(rel) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		mode, ok := modeOf(info)
		if !ok {
			return nil
		}
		ie, tracked := w.Index[rel]
		if !tracked {
			if !ig.match(rel, false) {
				out = append(out, FileStatus{Path: rel, Code: '?', Mode: mode})
			}
			return nil
		}
		seen[rel] = true
		if mode == ie.Mode && info.Size() == ie.Size && info.ModTime().UnixNano() == ie.MtimeNs {
			return nil
		}
		content, err := readContent(full, mode)
		if err != nil {
			return err
		}
		if mode == ie.Mode && object.Hash(object.KindBlob, content) == ie.Blob {
			ie.Size, ie.MtimeNs = info.Size(), info.ModTime().UnixNano()
			w.Index[rel] = ie
			return nil
		}
		out = append(out, FileStatus{Path: rel, Code: 'M', Mode: mode})
		return nil
	})
	if err != nil {
		return nil, err
	}
	for p, ie := range w.Index {
		if !seen[p] {
			out = append(out, FileStatus{Path: p, Code: 'D', Mode: ie.Mode})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (w *WC) tracksUnder(dir string) bool {
	for p := range w.Index {
		if strings.HasPrefix(p, dir+"/") {
			return true
		}
	}
	return false
}
