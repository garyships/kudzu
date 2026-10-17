package store

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/garyships/kudzu/internal/object"
)

// S3 stores objects in an S3-compatible bucket: AWS S3, Google Cloud
// Storage (its XML API, with HMAC keys), Cloudflare R2 or MinIO. Objects are
// immutable, so there is nothing to coordinate between servers sharing a
// bucket, and in front of it the server's memory cache can keep any object
// for as long as it likes.
//
// It speaks the handful of REST calls it needs (GET, HEAD, PUT, DELETE,
// server-side copy and ListObjectsV2) with its own Signature Version 4
// signer, so Kudzu does not depend on a cloud SDK.
type S3 struct {
	cfg S3Config
	hc  *http.Client
	now func() time.Time // judges object ages; tests move it
}

// S3Config locates a bucket and holds credentials.
type S3Config struct {
	Endpoint     string // scheme://host[:port]; default https://s3.REGION.amazonaws.com
	Bucket       string
	Prefix       string // key prefix, e.g. "kudzu/"
	Region       string // default us-east-1; "auto" for GCS and R2
	PathStyle    bool   // bucket in the path rather than the host name
	AccessKey    string // no key: unsigned requests
	SecretKey    string
	SessionToken string
}

// ParseObjectsURL parses s3://bucket/prefix and gs://bucket/prefix URLs.
// Query parameters: region, endpoint (for MinIO, R2 and other S3-compatible
// services) and path_style (true or false; default true when an endpoint is
// given). Credentials come from AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY and
// AWS_SESSION_TOKEN (for GCS, an HMAC key pair); the region defaults to
// AWS_REGION.
func ParseObjectsURL(raw string) (S3Config, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return S3Config{}, err
	}
	if u.Host == "" {
		return S3Config{}, fmt.Errorf("object store URL %q has no bucket", raw)
	}
	q := u.Query()
	cfg := S3Config{
		Bucket:       u.Host,
		Prefix:       strings.Trim(u.Path, "/"),
		Region:       q.Get("region"),
		Endpoint:     strings.TrimRight(q.Get("endpoint"), "/"),
		AccessKey:    os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretKey:    os.Getenv("AWS_SECRET_ACCESS_KEY"),
		SessionToken: os.Getenv("AWS_SESSION_TOKEN"),
	}
	if cfg.Prefix != "" {
		cfg.Prefix += "/"
	}
	switch u.Scheme {
	case "s3":
	case "gs":
		if cfg.Endpoint == "" {
			cfg.Endpoint = "https://storage.googleapis.com"
		}
		if cfg.Region == "" {
			cfg.Region = "auto"
		}
	default:
		return S3Config{}, fmt.Errorf("unsupported object store %q (use s3:// or gs://)", raw)
	}
	if cfg.Region == "" {
		cfg.Region = os.Getenv("AWS_REGION")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	cfg.PathStyle = cfg.Endpoint != ""
	if ps := q.Get("path_style"); ps != "" {
		if cfg.PathStyle, err = strconv.ParseBool(ps); err != nil {
			return S3Config{}, fmt.Errorf("bad path_style %q", ps)
		}
	}
	return cfg, nil
}

// OpenS3 returns a store for cfg and checks that the bucket is reachable.
func OpenS3(ctx context.Context, cfg S3Config) (*S3, error) {
	if cfg.Endpoint == "" {
		cfg.Endpoint = "https://s3." + cfg.Region + ".amazonaws.com"
	}
	if _, err := url.Parse(cfg.Endpoint); err != nil {
		return nil, fmt.Errorf("bad endpoint %q: %w", cfg.Endpoint, err)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConnsPerHost = 64
	tr.ResponseHeaderTimeout = time.Minute
	s := &S3{cfg: cfg, hc: &http.Client{Transport: tr}, now: time.Now}
	resp, err := s.do(ctx, "GET", "", url.Values{"list-type": {"2"}, "max-keys": {"1"}, "prefix": {cfg.Prefix}}, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("bucket %s: %w", cfg.Bucket, err)
	}
	resp.Body.Close()
	return s, nil
}

func (s *S3) key(id object.ID) string {
	h := id.String()
	return s.cfg.Prefix + h[:2] + "/" + h[2:]
}

func (s *S3) Get(ctx context.Context, id object.ID) ([]byte, error) {
	resp, err := s.do(ctx, "GET", s.key(id), nil, nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func (s *S3) Has(ctx context.Context, id object.ID) (bool, error) {
	_, err := s.head(ctx, s.key(id))
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (s *S3) Put(ctx context.Context, id object.ID, stored []byte) error {
	resp, err := s.do(ctx, "PUT", s.key(id), nil, stored, http.Header{"Content-Type": {"application/octet-stream"}})
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

type s3Head struct {
	size     int64
	modified time.Time
}

func (s *S3) head(ctx context.Context, key string) (s3Head, error) {
	resp, err := s.do(ctx, "HEAD", key, nil, nil, nil)
	if err != nil {
		return s3Head{}, err
	}
	resp.Body.Close()
	mod, err := http.ParseTime(resp.Header.Get("Last-Modified"))
	if err != nil {
		return s3Head{}, fmt.Errorf("%s: bad Last-Modified %q", key, resp.Header.Get("Last-Modified"))
	}
	return s3Head{resp.ContentLength, mod}, nil
}

// Touch refreshes an object's Last-Modified time with a server-side copy
// onto itself (no data moves), at most once per touchInterval.
func (s *S3) Touch(ctx context.Context, id object.ID) error {
	key := s.key(id)
	h, err := s.head(ctx, key)
	if err != nil {
		return err
	}
	if s.now().Sub(h.modified) <= touchInterval {
		return nil
	}
	resp, err := s.do(ctx, "PUT", key, nil, nil, http.Header{
		"X-Amz-Copy-Source":        {"/" + s.cfg.Bucket + "/" + escapePath(key)},
		"X-Amz-Metadata-Directive": {"REPLACE"},
		"Content-Type":             {"application/octet-stream"},
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// A copy can fail after the 200 status line; the error is in the body.
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if bytes.Contains(b, []byte("<Error>")) {
		return fmt.Errorf("touch %s: %s", key, s3ErrorMessage(b))
	}
	return nil
}

type listResult struct {
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
	Contents              []struct {
		Key          string    `xml:"Key"`
		LastModified time.Time `xml:"LastModified"`
		Size         int64     `xml:"Size"`
	} `xml:"Contents"`
}

func (s *S3) Walk(ctx context.Context, fn func(id object.ID, size int64, mtime time.Time) error) error {
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {s.cfg.Prefix}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := s.do(ctx, "GET", "", q, nil, nil)
		if err != nil {
			return err
		}
		var res listResult
		err = xml.NewDecoder(resp.Body).Decode(&res)
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("list %s: %w", s.cfg.Bucket, err)
		}
		for _, c := range res.Contents {
			id, err := object.ParseID(strings.ReplaceAll(strings.TrimPrefix(c.Key, s.cfg.Prefix), "/", ""))
			if err != nil || s.key(id) != c.Key {
				continue // not an object
			}
			if err := fn(id, c.Size, c.LastModified); err != nil {
				return err
			}
		}
		if !res.IsTruncated || res.NextContinuationToken == "" {
			return nil
		}
		token = res.NextContinuationToken
	}
}

func (s *S3) DeleteIfOlder(ctx context.Context, id object.ID, cutoff time.Time) (bool, int64, error) {
	key := s.key(id)
	h, err := s.head(ctx, key)
	if errors.Is(err, ErrNotFound) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	if !h.modified.Before(cutoff) {
		return false, 0, nil
	}
	resp, err := s.do(ctx, "DELETE", key, nil, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	resp.Body.Close()
	return true, h.size, nil
}

// do sends a signed request, retrying server errors and dropped connections
// (every call here is idempotent). A 404 is ErrNotFound.
func (s *S3) do(ctx context.Context, method, key string, q url.Values, body []byte, hdr http.Header) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt*attempt) * 100 * time.Millisecond):
			}
		}
		req, err := s.request(ctx, method, key, q, body, hdr)
		if err != nil {
			return nil, err
		}
		resp, err := s.hc.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
			continue
		}
		if resp.StatusCode/100 == 2 {
			return resp, nil
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound && key != "" {
			return nil, fmt.Errorf("%s: %w", key, ErrNotFound)
		}
		lastErr = fmt.Errorf("%s %s: %s: %s", method, s.cfg.Bucket+"/"+key, resp.Status, s3ErrorMessage(b))
		if resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
			return nil, lastErr
		}
	}
	return nil, lastErr
}

func s3ErrorMessage(b []byte) string {
	var e struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	if xml.Unmarshal(b, &e) == nil && e.Code != "" {
		return strings.TrimSpace(e.Code + " " + e.Message)
	}
	return strings.TrimSpace(string(b))
}

func (s *S3) request(ctx context.Context, method, key string, q url.Values, body []byte, hdr http.Header) (*http.Request, error) {
	ep, _ := url.Parse(s.cfg.Endpoint)
	host, path := ep.Host, "/"
	if s.cfg.PathStyle {
		path += s.cfg.Bucket
		if key != "" {
			path += "/" + key
		}
	} else {
		host = s.cfg.Bucket + "." + host
		path += key
	}
	rawPath := escapePath(path)
	u := ep.Scheme + "://" + host + rawPath
	if rq := canonicalQuery(q); rq != "" {
		u += "?" + rq
	}
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.ContentLength = int64(len(body))
	if body == nil {
		req.Body = http.NoBody
	}
	for k, vs := range hdr {
		req.Header[k] = vs
	}
	sum := sha256.Sum256(body)
	req.Header.Set("X-Amz-Content-Sha256", hex.EncodeToString(sum[:]))
	if s.cfg.AccessKey != "" {
		// The real time, whatever s.now says: S3 refuses a signature
		// more than 15 minutes off.
		signV4(req, s.cfg, rawPath, time.Now())
	}
	return req, nil
}

// signV4 adds an AWS Signature Version 4 Authorization header for S3.
// rawPath is the request path exactly as sent. It signs the host, every
// x-amz-* header and Range.
func signV4(req *http.Request, cfg S3Config, rawPath string, now time.Time) {
	now = now.UTC()
	amzDate := now.Format("20060102T150405Z")
	day := now.Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)
	if cfg.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", cfg.SessionToken)
	}
	headers := map[string]string{"host": req.URL.Host}
	if req.Host != "" {
		headers["host"] = req.Host
	}
	for k, vs := range req.Header {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-amz-") || lk == "range" || lk == "content-md5" {
			headers[lk] = strings.Join(vs, ",")
		}
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canon strings.Builder
	for _, k := range names {
		canon.WriteString(k + ":" + strings.TrimSpace(headers[k]) + "\n")
	}
	signed := strings.Join(names, ";")
	creq := strings.Join([]string{
		req.Method,
		rawPath,
		canonicalQuery(req.URL.Query()),
		canon.String(),
		signed,
		req.Header.Get("X-Amz-Content-Sha256"),
	}, "\n")
	scope := day + "/" + cfg.Region + "/s3/aws4_request"
	h := sha256.Sum256([]byte(creq))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(h[:])
	key := hmacSHA256([]byte("AWS4"+cfg.SecretKey), day)
	key = hmacSHA256(key, cfg.Region)
	key = hmacSHA256(key, "s3")
	key = hmacSHA256(key, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(key, toSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+cfg.AccessKey+"/"+scope+", SignedHeaders="+signed+", Signature="+sig)
}

func hmacSHA256(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}

// escapePath percent-encodes everything but unreserved characters and '/',
// as SigV4 requires for S3.
func escapePath(p string) string { return uriEncode(p, false) }

func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, uriEncode(k, true)+"="+uriEncode(v, true))
		}
	}
	return strings.Join(parts, "&")
}

func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
