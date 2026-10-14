package object

import (
	"encoding/json"
	"fmt"
)

// Change is one file changed relative to a changeset's first parent. Mode 0
// means the file was deleted.
type Change struct {
	Path string `json:"path"`
	Mode uint32 `json:"mode"`
	Blob ID     `json:"blob"`
}

func (c Change) Deleted() bool { return c.Mode == 0 }

// Changeset is the unit of work: an entry in the write-ahead log. Unlike a git
// commit it does not point at a root tree; it lists the files that changed
// relative to its first parent. Root trees, path history, blame and the
// commit-graph skip list are all derived data computed from changesets.
type Changeset struct {
	Parents []ID              `json:"parents"`
	Author  string            `json:"author"`
	Time    int64             `json:"time"`
	TZ      int               `json:"tz"` // minutes east of UTC
	Message string            `json:"message"`
	Changes []Change          `json:"changes"`
	Extra   map[string]string `json:"extra,omitempty"`
}

// Encode returns the canonical encoding the changeset ID is computed from.
func (c *Changeset) Encode() []byte {
	cc := *c
	if cc.Parents == nil {
		cc.Parents = []ID{}
	}
	if cc.Changes == nil {
		cc.Changes = []Change{}
	}
	b, err := json.Marshal(&cc)
	if err != nil {
		panic(err)
	}
	return b
}

func (c *Changeset) ID() ID { return Hash(KindChangeset, c.Encode()) }

func DecodeChangeset(b []byte) (*Changeset, error) {
	var c Changeset
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("decode changeset: %w", err)
	}
	return &c, nil
}
