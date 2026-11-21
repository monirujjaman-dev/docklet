package s3

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func TestUnitURLsAndSigning(t *testing.T) {
	c, err := New(Config{Endpoint: "https://s3.example.com/", Bucket: "my-bucket", Prefix: "/prod/db/", AccessKey: "AK", SecretKey: "SK"})
	if err != nil {
		t.Fatal(err)
	}
	u, p := c.objectURL(c.FullKey("1/2026 11 21+dump.gz"), url.Values{"partNumber": {"1"}, "uploadId": {"a/b"}})
	if u.Host != "my-bucket.s3.example.com" || p != "/prod/db/1/2026%2011%2021%2Bdump.gz" || u.RawQuery != "partNumber=1&uploadId=a%2Fb" {
		t.Fatalf("virtual-hosted: %s %s %s", u.Host, p, u.RawQuery)
	}
	c.cfg.PathStyle = true
	if _, p := c.objectURL("k", nil); p != "/my-bucket/k" {
		t.Fatalf("path style: %s", p)
	}
	for _, bad := range []Config{
		{Endpoint: "ftp://x", Bucket: "abc", AccessKey: "a", SecretKey: "b"},
		{Endpoint: "https://x/path", Bucket: "abc", AccessKey: "a", SecretKey: "b"},
		{Endpoint: "https://x", Bucket: "AB", AccessKey: "a", SecretKey: "b"},
		{Endpoint: "https://x", Bucket: "abc"},
	} {
		if _, err := New(bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

// A fake server checks the request shape and the multipart flow.
func TestUnitMultipartFlow(t *testing.T) {
	var parts [][]byte
	var completed string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AK/") {
			http.Error(w, "unsigned", http.StatusForbidden)
			return
		}
		q := r.URL.Query()
		switch {
		case r.Method == "POST" && q.Has("uploads"):
			_, _ = io.WriteString(w, `<InitiateMultipartUploadResult><UploadId>U1</UploadId></InitiateMultipartUploadResult>`)
		case r.Method == "PUT" && q.Get("uploadId") == "U1":
			b, _ := io.ReadAll(r.Body)
			sum := sha256.Sum256(b)
			if r.Header.Get("X-Amz-Content-Sha256") != hexs(sum[:]) {
				http.Error(w, "bad hash", 400)
				return
			}
			parts = append(parts, b)
			w.Header().Set("ETag", `"e`+q.Get("partNumber")+`"`)
		case r.Method == "POST" && q.Get("uploadId") == "U1":
			b, _ := io.ReadAll(r.Body)
			completed = string(b)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.String(), 400)
		}
	}))
	defer srv.Close()
	c, _ := New(Config{Endpoint: srv.URL, Bucket: "bkt", AccessKey: "AK", SecretKey: "SK", PathStyle: true})
	c.PartSize = 5 << 20
	data := make([]byte, 12<<20)
	_, _ = rand.Read(data)
	if err := c.Upload(context.Background(), "big", bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if len(parts) != 3 || !bytes.Equal(bytes.Join(parts, nil), data) {
		t.Fatalf("parts = %d", len(parts))
	}
	if !strings.Contains(completed, "<PartNumber>3</PartNumber><ETag>&#34;e3&#34;</ETag>") {
		t.Fatalf("complete body = %s", completed)
	}
}

func hexs(b []byte) string { return hex.EncodeToString(b) }

// TestReal runs against a real S3-compatible service:
//
//	DOCKLET_TEST_S3_ENDPOINT=http://127.0.0.1:8333 DOCKLET_TEST_S3_BUCKET=test \
//	DOCKLET_TEST_S3_ACCESS_KEY=… DOCKLET_TEST_S3_SECRET_KEY=… [DOCKLET_TEST_S3_REGION=…] \
//	[DOCKLET_TEST_S3_PATH_STYLE=1] go test ./internal/s3/ -run TestReal -v
func TestReal(t *testing.T) {
	ep := os.Getenv("DOCKLET_TEST_S3_ENDPOINT")
	if ep == "" {
		t.Skip("set DOCKLET_TEST_S3_ENDPOINT to run against a real S3 service")
	}
	cfg := Config{
		Endpoint: ep, Bucket: os.Getenv("DOCKLET_TEST_S3_BUCKET"), Region: os.Getenv("DOCKLET_TEST_S3_REGION"),
		AccessKey: os.Getenv("DOCKLET_TEST_S3_ACCESS_KEY"), SecretKey: os.Getenv("DOCKLET_TEST_S3_SECRET_KEY"),
		PathStyle: os.Getenv("DOCKLET_TEST_S3_PATH_STYLE") == "1", Prefix: "docklet-test/" + time.Now().Format("150405"),
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.Test(ctx); err != nil {
		t.Fatalf("Test(): %v", err)
	}

	c.PartSize = 5 << 20
	big := make([]byte, 13<<20+123)
	_, _ = rand.Read(big)
	start := time.Now()
	if err := c.Upload(ctx, "dumps/big.bin", bytes.NewReader(big), int64(len(big))); err != nil {
		t.Fatalf("multipart upload: %v", err)
	}
	t.Logf("13 MB multipart upload in %s", time.Since(start).Round(time.Millisecond))
	if err := c.Upload(ctx, "dumps/small file+1.txt", strings.NewReader("hello"), 5); err != nil {
		t.Fatalf("small upload: %v", err)
	}
	body, n, err := c.Get(ctx, "dumps/big.bin")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(body)
	_ = body.Close()
	if !bytes.Equal(got, big) || n != int64(len(big)) {
		t.Fatalf("downloaded %d bytes (header %d), equal=%v", len(got), n, bytes.Equal(got, big))
	}
	objs, err := c.List(ctx, "dumps/")
	if err != nil || len(objs) != 2 || objs[0].Key != "dumps/big.bin" || objs[1].Key != "dumps/small file+1.txt" || objs[0].Size != int64(len(big)) {
		t.Fatalf("list = %+v, %v", objs, err)
	}
	for _, o := range objs {
		if err := c.Delete(ctx, o.Key); err != nil {
			t.Fatal(err)
		}
	}
	if objs, _ := c.List(ctx, "dumps/"); len(objs) != 0 {
		t.Fatalf("after delete: %+v", objs)
	}
	if _, _, err := c.Get(ctx, "dumps/big.bin"); !IsNotFound(err) {
		t.Fatalf("get deleted: %v", err)
	}

	wrong := cfg
	wrong.SecretKey = "wrong-secret"
	wc, _ := New(wrong)
	err = wc.Test(ctx)
	if err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("wrong secret: %v", err)
	}
	t.Logf("wrong secret -> %v", err)
	nb := cfg
	nb.Bucket = "docklet-no-such-bucket-xyz"
	nc, _ := New(nb)
	err = nc.Test(ctx)
	if err == nil {
		t.Fatal("missing bucket accepted")
	}
	t.Logf("missing bucket -> %v", err)
}
