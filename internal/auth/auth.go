// Package auth implements bearer-token authentication and per-directory
// access control for the server. A policy file maps tokens to principals,
// and each principal to path-prefix rules:
//
//	{
//	  "principals": {
//	    "alice": {
//	      "token_sha256": ["<hex sha-256 of alice's token>"],
//	      "read":  [""],
//	      "write": ["services/api"],
//	      "deny":  ["secrets"],
//	      "land":  ["main", "users/alice/*"]
//	    }
//	  },
//	  "anonymous": {"read": ["docs"]}
//	}
//
// For any path the most specific (longest) matching prefix decides, and at
// equal length deny beats write, which beats read. "" is the whole
// repository; write implies read. Land rules name refs a principal may move
// (by landing or by compare-and-swap), with a trailing * matching any
// suffix. Requests without a token act as "anonymous" if the policy has
// one, and are rejected otherwise.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/garyships/kudzu/internal/object"
)

// Level is what a principal may do with a path.
type Level int

const (
	None Level = iota
	Read
	Write
)

func (l Level) String() string {
	return [...]string{"none", "read", "write"}[l]
}

// DeniedError is a request a principal is not allowed to make. The server
// reports it as 403, naming the path or ref.
type DeniedError struct {
	Principal string
	Action    string // read, write, land or upload
	Path      string // a path, or a ref for land
	Hidden    bool   // the path is one the principal may not read, so it is not named
}

func (e *DeniedError) Error() string {
	if e.Hidden {
		return fmt.Sprintf("permission denied: this would change paths %s may not read", e.Principal)
	}
	what := e.Path
	if what == "" {
		what = "the repository root"
	}
	switch e.Action {
	case "land":
		return fmt.Sprintf("permission denied: %s may not move ref %s", e.Principal, what)
	case "upload":
		return fmt.Sprintf("permission denied: %s may not upload objects (it may not write anywhere)", e.Principal)
	}
	return fmt.Sprintf("permission denied: %s may not %s %s", e.Principal, e.Action, what)
}

// ErrUnauthenticated means a request had no valid token.
var ErrUnauthenticated = errors.New("authentication required")

type rule struct {
	prefix string
	level  Level
	deny   bool
}

// Principal is an authenticated caller and its rules. A nil *Principal
// (auth disabled) may do anything.
type Principal struct {
	Name  string
	rules []rule // sorted by prefix length, longest first
	land  []string
}

type principalConfig struct {
	TokenSHA256 []string `json:"token_sha256"`
	Read        []string `json:"read"`
	Write       []string `json:"write"`
	Deny        []string `json:"deny"`
	Land        []string `json:"land"`
}

type config struct {
	Principals map[string]*principalConfig `json:"principals"`
	Anonymous  *principalConfig            `json:"anonymous"`
}

// Policy authenticates tokens.
type Policy struct {
	byToken   map[[sha256.Size]byte]*Principal
	anonymous *Principal
}

// Load reads a policy file.
func Load(path string) (*Policy, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	p, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

// Parse parses a policy (see the package comment for the format).
func Parse(b []byte) (*Policy, error) {
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	var c config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("bad auth policy: %w", err)
	}
	p := &Policy{byToken: map[[sha256.Size]byte]*Principal{}}
	names := make([]string, 0, len(c.Principals))
	for name := range c.Principals {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		pc := c.Principals[name]
		if name == "" || name == "anonymous" {
			return nil, fmt.Errorf("bad auth policy: %q is not a valid principal name", name)
		}
		pr, err := newPrincipal(name, pc)
		if err != nil {
			return nil, err
		}
		if len(pc.TokenSHA256) == 0 {
			return nil, fmt.Errorf("bad auth policy: principal %s has no token_sha256", name)
		}
		for _, h := range pc.TokenSHA256 {
			raw, err := hex.DecodeString(strings.TrimSpace(h))
			if err != nil || len(raw) != sha256.Size {
				return nil, fmt.Errorf("bad auth policy: principal %s: token_sha256 %q is not a hex SHA-256 (see 'kudzu token')", name, h)
			}
			var k [sha256.Size]byte
			copy(k[:], raw)
			if other, ok := p.byToken[k]; ok {
				return nil, fmt.Errorf("bad auth policy: principals %s and %s share a token", other.Name, name)
			}
			p.byToken[k] = pr
		}
	}
	if c.Anonymous != nil {
		if len(c.Anonymous.TokenSHA256) > 0 {
			return nil, errors.New("bad auth policy: anonymous cannot have tokens")
		}
		pr, err := newPrincipal("anonymous", c.Anonymous)
		if err != nil {
			return nil, err
		}
		p.anonymous = pr
	}
	return p, nil
}

func newPrincipal(name string, pc *principalConfig) (*Principal, error) {
	pr := &Principal{Name: name}
	add := func(prefixes []string, level Level, deny bool) error {
		for _, raw := range prefixes {
			prefix := strings.Trim(strings.TrimSpace(raw), "/")
			if prefix == "." {
				prefix = ""
			}
			if prefix != "" {
				if err := object.ValidatePath(prefix); err != nil {
					return fmt.Errorf("bad auth policy: principal %s: %w", name, err)
				}
			}
			pr.rules = append(pr.rules, rule{prefix, level, deny})
		}
		return nil
	}
	if err := add(pc.Read, Read, false); err != nil {
		return nil, err
	}
	if err := add(pc.Write, Write, false); err != nil {
		return nil, err
	}
	if err := add(pc.Deny, None, true); err != nil {
		return nil, err
	}
	// Longest prefix first; at equal length deny, then write, then read.
	sort.SliceStable(pr.rules, func(i, j int) bool {
		a, b := pr.rules[i], pr.rules[j]
		if len(a.prefix) != len(b.prefix) {
			return len(a.prefix) > len(b.prefix)
		}
		if a.deny != b.deny {
			return a.deny
		}
		return a.level > b.level
	})
	for _, ref := range pc.Land {
		if ref = strings.TrimSpace(ref); ref == "" {
			return nil, fmt.Errorf("bad auth policy: principal %s: empty land ref", name)
		}
		pr.land = append(pr.land, ref)
	}
	return pr, nil
}

// Authenticate returns the principal for a bearer token ("" for none).
func (p *Policy) Authenticate(token string) (*Principal, error) {
	if token == "" {
		if p.anonymous != nil {
			return p.anonymous, nil
		}
		return nil, ErrUnauthenticated
	}
	if pr, ok := p.byToken[sha256.Sum256([]byte(token))]; ok {
		return pr, nil
	}
	return nil, fmt.Errorf("%w: unknown token", ErrUnauthenticated)
}

// NewToken returns a random token and the hash to put in a policy file.
func NewToken() (token, hash string, err error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", err
	}
	token = "kzt_" + base64.RawURLEncoding.EncodeToString(b[:])
	sum := sha256.Sum256([]byte(token))
	return token, hex.EncodeToString(sum[:]), nil
}

func under(path, prefix string) bool {
	return prefix == "" || path == prefix || strings.HasPrefix(path, prefix+"/")
}

// Level returns what the principal may do with path.
func (pr *Principal) Level(path string) Level {
	if pr == nil {
		return Write
	}
	for _, r := range pr.rules {
		if under(path, r.prefix) {
			return r.level
		}
	}
	return None
}

// CanRead reports whether the principal may read path (a file, or a whole
// directory unless something below it is denied).
func (pr *Principal) CanRead(path string) bool { return pr.Level(path) >= Read }

// CanWrite reports whether the principal may change path.
func (pr *Principal) CanWrite(path string) bool { return pr.Level(path) >= Write }

// CanSee reports whether the principal may list directory dir: it can read
// dir, or something below it.
func (pr *Principal) CanSee(dir string) bool {
	if pr == nil || pr.CanRead(dir) {
		return true
	}
	for _, r := range pr.rules {
		if !r.deny && r.prefix != dir && under(r.prefix, dir) && pr.Level(r.prefix) >= Read {
			return true
		}
	}
	return false
}

// ReadsAll reports whether the principal may read everything below dir,
// with no exception further down.
func (pr *Principal) ReadsAll(dir string) bool {
	if pr == nil {
		return true
	}
	if !pr.CanRead(dir) {
		return false
	}
	for _, r := range pr.rules {
		if r.level < Read && r.prefix != dir && under(r.prefix, dir) {
			return false
		}
	}
	return true
}

// WritesAll reports whether the principal may change everything below dir.
func (pr *Principal) WritesAll(dir string) bool {
	if pr == nil {
		return true
	}
	if !pr.CanWrite(dir) {
		return false
	}
	for _, r := range pr.rules {
		if r.level < Write && r.prefix != dir && under(r.prefix, dir) {
			return false
		}
	}
	return true
}

// WritesAnywhere reports whether the principal may change any path at all
// (and so upload objects).
func (pr *Principal) WritesAnywhere() bool {
	if pr == nil {
		return true
	}
	for _, r := range pr.rules {
		if r.level >= Write {
			return true
		}
	}
	return false
}

// CanLand reports whether the principal may move ref.
func (pr *Principal) CanLand(ref string) bool {
	if pr == nil {
		return true
	}
	for _, l := range pr.land {
		if l == ref || (strings.HasSuffix(l, "*") && strings.HasPrefix(ref, strings.TrimSuffix(l, "*"))) {
			return true
		}
	}
	return false
}

// CheckRead, CheckWrite and CheckLand return a *DeniedError or nil.
func (pr *Principal) CheckRead(path string) error {
	if pr.CanRead(path) {
		return nil
	}
	return &DeniedError{Principal: pr.Name, Action: "read", Path: path}
}

// CheckSee is CheckRead for directories that are listed or searched.
func (pr *Principal) CheckSee(dir string) error {
	if pr.CanSee(dir) {
		return nil
	}
	return &DeniedError{Principal: pr.Name, Action: "read", Path: dir}
}

func (pr *Principal) CheckWrite(path string) error {
	if pr.CanWrite(path) {
		return nil
	}
	return &DeniedError{Principal: pr.Name, Action: "write", Path: path}
}

func (pr *Principal) CheckLand(ref string) error {
	if pr.CanLand(ref) {
		return nil
	}
	return &DeniedError{Principal: pr.Name, Action: "land", Path: ref}
}

type ctxKey struct{}

// WithPrincipal attaches a principal to a request context.
func WithPrincipal(ctx context.Context, pr *Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, pr)
}

// FromContext returns the request's principal, or nil when auth is off.
func FromContext(ctx context.Context) *Principal {
	pr, _ := ctx.Value(ctxKey{}).(*Principal)
	return pr
}
