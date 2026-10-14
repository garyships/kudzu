package store

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestSignV4Vectors checks the signer against the worked examples in the
// Amazon S3 documentation ("Signature calculations for the Authorization
// header").
func TestSignV4Vectors(t *testing.T) {
	cfg := S3Config{Region: "us-east-1", AccessKey: "AKIAIOSFODNN7EXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}
	when := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	empty := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	for _, c := range []struct {
		name, method, url, rawPath string
		headers                    map[string]string
		want                       string
	}{
		{"GET object", "GET", "https://examplebucket.s3.amazonaws.com/test.txt", "/test.txt",
			map[string]string{"Range": "bytes=0-9", "X-Amz-Content-Sha256": empty},
			"f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"},
		{"GET bucket", "GET", "https://examplebucket.s3.amazonaws.com/?max-keys=2&prefix=J", "/",
			map[string]string{"X-Amz-Content-Sha256": empty},
			"34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7"},
	} {
		req, _ := http.NewRequest(c.method, c.url, nil)
		for k, v := range c.headers {
			req.Header.Set(k, v)
		}
		signV4(req, cfg, c.rawPath, when)
		if got := req.Header.Get("Authorization"); !strings.HasSuffix(got, "Signature="+c.want) {
			t.Errorf("%s: %s", c.name, got)
		}
	}
}
