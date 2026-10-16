package object

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const (
	ModeFile    uint32 = 0o100644
	ModeExec    uint32 = 0o100755
	ModeSymlink uint32 = 0o120000
	ModeGitlink uint32 = 0o160000
	ModeDir     uint32 = 0o040000
)

// ValidFileMode reports whether m is a mode a file entry may have.
func ValidFileMode(m uint32) bool {
	switch m {
	case ModeFile, ModeExec, ModeSymlink, ModeGitlink:
		return true
	}
	return false
}

// IsRegular reports whether m is a regular (possibly executable) file.
func IsRegular(m uint32) bool { return m == ModeFile || m == ModeExec }

// Entry is one name in a directory. For trees, ID is a blob or subtree; for
// directory unodes, it is a child unode.
type Entry struct {
	Name string `json:"name"`
	Mode uint32 `json:"mode"`
	ID   ID     `json:"id"`
}

func (e Entry) IsDir() bool { return e.Mode == ModeDir }

// Tree is a directory snapshot. Identical directories hash identically across
// all changesets, which is what makes want-based fetching cheap: a client
// never asks for a subtree it already has.
type Tree struct {
	Entries []Entry
}

var EmptyTreeID = Hash(KindTree, nil)

func SortEntries(es []Entry) {
	sort.Slice(es, func(i, j int) bool { return es[i].Name < es[j].Name })
}

func encodeEntries(b *bytes.Buffer, es []Entry) {
	for _, e := range es {
		b.WriteString(strconv.FormatUint(uint64(e.Mode), 8))
		b.WriteByte(' ')
		b.WriteString(e.Name)
		b.WriteByte(0)
		b.Write(e.ID[:])
	}
}

func decodeEntries(b []byte) ([]Entry, error) {
	var es []Entry
	for len(b) > 0 {
		sp := bytes.IndexByte(b, ' ')
		if sp < 0 {
			return nil, errors.New("tree: missing mode separator")
		}
		mode, err := strconv.ParseUint(string(b[:sp]), 8, 32)
		if err != nil {
			return nil, fmt.Errorf("tree: bad mode: %w", err)
		}
		b = b[sp+1:]
		nul := bytes.IndexByte(b, 0)
		if nul < 0 || len(b) < nul+1+IDSize {
			return nil, errors.New("tree: truncated entry")
		}
		e := Entry{Name: string(b[:nul]), Mode: uint32(mode)}
		if !validName(e.Name) {
			// Never trust names from the wire: "../x" or "a/b" would let a
			// hostile tree write outside a working copy.
			return nil, fmt.Errorf("tree: invalid entry name %q", e.Name)
		}
		if len(es) > 0 && e.Name <= es[len(es)-1].Name {
			// Entries are always written sorted (SortEntries). A repeated
			// name could make a checkout replace a directory with a symlink
			// and then write through it; lookups and tree diffs need the
			// order too.
			return nil, fmt.Errorf("tree: entry %q is out of order or repeated", e.Name)
		}
		copy(e.ID[:], b[nul+1:nul+1+IDSize])
		es = append(es, e)
		b = b[nul+1+IDSize:]
	}
	return es, nil
}

func validName(n string) bool {
	return n != "" && n != "." && n != ".." && !strings.ContainsAny(n, "/\x00")
}

func (t *Tree) Encode() []byte {
	var b bytes.Buffer
	encodeEntries(&b, t.Entries)
	return b.Bytes()
}

func DecodeTree(b []byte) (*Tree, error) {
	es, err := decodeEntries(b)
	if err != nil {
		return nil, err
	}
	return &Tree{Entries: es}, nil
}

func (t *Tree) Lookup(name string) (Entry, bool) {
	return lookupEntry(t.Entries, name)
}

func lookupEntry(es []Entry, name string) (Entry, bool) {
	i := sort.Search(len(es), func(i int) bool { return es[i].Name >= name })
	if i < len(es) && es[i].Name == name {
		return es[i], true
	}
	return Entry{}, false
}

// Unode is a history-aware node ("unode" in Mononoke's terms). Unlike tree
// entries, a unode records the changeset that produced it (Linknode) and the
// previous version at the same path, so path history and blame walk only the
// versions of one path instead of the whole commit graph.
type Unode struct {
	Dir      bool
	Linknode ID
	Parents  []ID
	Mode     uint32  // files only
	Content  ID      // files only: blob ID
	Entries  []Entry // dirs only: subdir entries have ModeDir, file entries their file mode
}

func (u *Unode) Encode() []byte {
	var b bytes.Buffer
	if u.Dir {
		b.WriteByte(1)
	} else {
		b.WriteByte(0)
	}
	b.Write(u.Linknode[:])
	var tmp [binary.MaxVarintLen64]byte
	b.Write(tmp[:binary.PutUvarint(tmp[:], uint64(len(u.Parents)))])
	for _, p := range u.Parents {
		b.Write(p[:])
	}
	if u.Dir {
		encodeEntries(&b, u.Entries)
	} else {
		b.Write(tmp[:binary.PutUvarint(tmp[:], uint64(u.Mode))])
		b.Write(u.Content[:])
	}
	return b.Bytes()
}

func DecodeUnode(b []byte) (*Unode, error) {
	if len(b) < 1+IDSize {
		return nil, errors.New("unode: truncated")
	}
	u := &Unode{Dir: b[0] == 1}
	copy(u.Linknode[:], b[1:1+IDSize])
	b = b[1+IDSize:]
	n, k := binary.Uvarint(b)
	if k <= 0 || uint64(len(b)-k) < n*IDSize {
		return nil, errors.New("unode: bad parents")
	}
	b = b[k:]
	for i := uint64(0); i < n; i++ {
		var p ID
		copy(p[:], b[:IDSize])
		u.Parents = append(u.Parents, p)
		b = b[IDSize:]
	}
	if u.Dir {
		es, err := decodeEntries(b)
		if err != nil {
			return nil, err
		}
		u.Entries = es
		return u, nil
	}
	mode, k := binary.Uvarint(b)
	if k <= 0 || len(b)-k != IDSize {
		return nil, errors.New("unode: bad file payload")
	}
	u.Mode = uint32(mode)
	copy(u.Content[:], b[k:])
	return u, nil
}

func (u *Unode) Lookup(name string) (Entry, bool) { return lookupEntry(u.Entries, name) }

// ValidatePath checks a repository-relative, slash-separated file path.
func ValidatePath(p string) error {
	if p == "" || len(p) > 4096 {
		return fmt.Errorf("invalid path %q", p)
	}
	if strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") {
		return fmt.Errorf("invalid path %q: leading or trailing slash", p)
	}
	reserved := ""
	for _, c := range strings.Split(p, "/") {
		if c == "" || c == "." || c == ".." || strings.IndexByte(c, 0) >= 0 {
			return fmt.Errorf("invalid path %q", p)
		}
		if reserved == "" && ReservedName(c) {
			reserved = c
		}
	}
	if reserved != "" {
		return fmt.Errorf("invalid path %q: %s is %w", p, reserved, ErrReserved)
	}
	return nil
}

// ErrReserved is wrapped by ValidatePath's error for a path that is well
// formed but goes through a reserved name.
var ErrReserved = errors.New("reserved")

// ReservedName reports whether a path component, at any depth, would be
// taken for .git or .kudzu: a nested .git/config can make git run commands,
// and .kudzu holds a working copy's state. Case is ignored, as Windows and
// macOS ignore it, and so are trailing dots and spaces, which Windows drops;
// git~1 and kudzu~1 are the NTFS short names.
func ReservedName(name string) bool {
	switch strings.ToLower(strings.TrimRight(name, ". ")) {
	case ".git", ".kudzu", "git~1", "kudzu~1":
		return true
	}
	return false
}

// JoinPath joins a directory path and a name ("" is the root).
func JoinPath(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}
