package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeS3 is a path-style S3 endpoint with just the calls the S3 store uses.
// It recomputes every request's signature from what arrived on the wire, so
// a mismatch between what the client signed and what it sent fails loudly.
type fakeS3 struct {
	t        *testing.T
	cfg      S3Config
	pageSize int
	mu       sync.Mutex
	now      time.Time
	objs     map[string]fakeObj
	requests map[string]int
}

type fakeObj struct {
	data []byte
	mod  time.Time
}

func newFakeS3(t *testing.T) (*fakeS3, S3Config) {
	f := &fakeS3{t: t, pageSize: 3, now: time.Now().Truncate(time.Second), objs: map[string]fakeObj{}, requests: map[string]int{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	f.cfg = S3Config{Endpoint: srv.URL, Bucket: "bkt", Prefix: "repo/", Region: "us-east-1", PathStyle: true, AccessKey: "AK", SecretKey: "SK"}
	return f, f.cfg
}

func (f *fakeS3) advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.mu.Unlock()
}

func (f *fakeS3) fail(w http.ResponseWriter, status int, code string) {
	w.WriteHeader(status)
	fmt.Fprintf(w, "<Error><Code>%s</Code><Message>fake</Message></Error>", code)
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests[r.Method]++
	// Verify the signature against the request as received.
	when, err := time.Parse("20060102T150405Z", r.Header.Get("X-Amz-Date"))
	if err != nil {
		f.fail(w, 403, "MissingDate")
		return
	}
	check, _ := http.NewRequest(r.Method, "http://"+r.Host+r.URL.RequestURI(), nil)
	for k, v := range r.Header {
		if k != "Authorization" && k != "X-Amz-Date" {
			check.Header[k] = v
		}
	}
	check.Host = r.Host
	signV4(check, f.cfg, r.URL.EscapedPath(), when)
	if got, want := r.Header.Get("Authorization"), check.Header.Get("Authorization"); got != want {
		f.t.Errorf("signature mismatch for %s %s:\n got  %s\n want %s", r.Method, r.URL.RequestURI(), got, want)
		f.fail(w, 403, "SignatureDoesNotMatch")
		return
	}
	body, _ := io.ReadAll(r.Body)
	if sum := sha256.Sum256(body); r.Header.Get("X-Amz-Content-Sha256") != hex.EncodeToString(sum[:]) {
		f.fail(w, 400, "XAmzContentSHA256Mismatch")
		return
	}
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if bucket != f.cfg.Bucket {
		f.fail(w, 404, "NoSuchBucket")
		return
	}
	switch {
	case key == "" && r.Method == "GET":
		f.list(w, r)
	case r.Method == "PUT" && r.Header.Get("X-Amz-Copy-Source") != "":
		src := strings.TrimPrefix(r.Header.Get("X-Amz-Copy-Source"), "/"+bucket+"/")
		o, ok := f.objs[src]
		if !ok {
			// Real S3 reports some copy failures in a 200 response body.
			fmt.Fprint(w, "<Error><Code>NoSuchKey</Code></Error>")
			return
		}
		f.objs[key] = fakeObj{o.data, f.now}
		fmt.Fprint(w, "<CopyObjectResult></CopyObjectResult>")
	case r.Method == "PUT":
		f.objs[key] = fakeObj{body, f.now}
	case r.Method == "GET" || r.Method == "HEAD":
		o, ok := f.objs[key]
		if !ok {
			f.fail(w, 404, "NoSuchKey")
			return
		}
		w.Header().Set("Last-Modified", o.mod.UTC().Format(http.TimeFormat))
		w.Header().Set("Content-Length", strconv.Itoa(len(o.data)))
		if r.Method == "GET" {
			w.Write(o.data)
		}
	case r.Method == "DELETE":
		delete(f.objs, key)
		w.WriteHeader(204)
	default:
		f.fail(w, 405, "MethodNotAllowed")
	}
}

func (f *fakeS3) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var keys []string
	for k := range f.objs {
		if strings.HasPrefix(k, q.Get("prefix")) && k > q.Get("continuation-token") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	type content struct {
		Key          string
		LastModified string
		Size         int
	}
	var res struct {
		XMLName               xml.Name `xml:"ListBucketResult"`
		IsTruncated           bool
		NextContinuationToken string `xml:",omitempty"`
		Contents              []content
	}
	n := f.pageSize
	if m, err := strconv.Atoi(q.Get("max-keys")); err == nil && m < n {
		n = m
	}
	if len(keys) > n {
		keys, res.IsTruncated, res.NextContinuationToken = keys[:n], true, keys[n-1]
	}
	for _, k := range keys {
		res.Contents = append(res.Contents, content{k, f.objs[k].mod.UTC().Format("2006-01-02T15:04:05.000Z"), len(f.objs[k].data)})
	}
	xml.NewEncoder(w).Encode(res)
}
