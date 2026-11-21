// Package s3 is a small S3-compatible client (AWS Signature V4) covering
// what Docklet needs for backups: put (streaming multipart for large files),
// get, list, delete and a connection test. It has no dependencies beyond the
// standard library.
package s3

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Config describes a bucket.
type Config struct {
	Endpoint  string // e.g. https://s3.eu-central-1.amazonaws.com, https://<acct>.r2.cloudflarestorage.com
	Region    string // default us-east-1
	Bucket    string
	Prefix    string // optional key prefix ("backups/prod")
	AccessKey string
	SecretKey string
	PathStyle bool // endpoint/bucket/key instead of bucket.endpoint/key
}

// Object is a listed object.
type Object struct {
	Key          string    `json:"key"` // relative to the prefix
	Size         int64     `json:"size"`
	LastModified time.Time `json:"lastModified"`
}

// Error is an S3 error response.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("S3: HTTP %d", e.Status)
	}
	if e.Message == "" {
		return "S3: " + e.Code
	}
	return fmt.Sprintf("S3: %s: %s", e.Code, e.Message)
}

// IsNotFound reports a missing key or bucket.
func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && (e.Status == http.StatusNotFound || e.Code == "NoSuchKey")
}

// Client talks to one bucket.
type Client struct {
	cfg       Config
	base      *url.URL
	HTTP      *http.Client
	now       func() time.Time
	PartSize  int64         // multipart part size (default 16 MiB, min 5 MiB)
	RetryWait time.Duration // base delay between retries of failed requests
}

// New validates cfg and returns a client.
func New(cfg Config) (*Client, error) {
	cfg.Endpoint = strings.TrimRight(strings.TrimSpace(cfg.Endpoint), "/")
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
		return nil, errors.New("endpoint must be a plain URL like https://s3.example.com")
	}
	if !validBucket(cfg.Bucket) {
		return nil, errors.New("bucket name must be 3-63 characters: lowercase letters, digits, '.', '-'")
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("access key and secret key are required")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	cfg.Prefix = strings.Trim(cfg.Prefix, "/")
	return &Client{cfg: cfg, base: u, HTTP: &http.Client{Timeout: 0}, now: time.Now, PartSize: 16 << 20, RetryWait: time.Second}, nil
}

func validBucket(b string) bool {
	if len(b) < 3 || len(b) > 63 {
		return false
	}
	for i := 0; i < len(b); i++ {
		c := b[i]
		lower, digit := c >= 'a' && c <= 'z', c >= '0' && c <= '9'
		if !lower && !digit && c != '.' && c != '-' {
			return false
		}
	}
	return b[0] != '.' && b[0] != '-' && b[len(b)-1] != '.' && b[len(b)-1] != '-'
}

// FullKey joins the configured prefix and key.
func (c *Client) FullKey(key string) string {
	key = strings.TrimLeft(key, "/")
	if c.cfg.Prefix == "" {
		return key
	}
	return c.cfg.Prefix + "/" + key
}

func (c *Client) objectURL(key string, q url.Values) (*url.URL, string) {
	u := *c.base
	path := "/"
	if c.cfg.PathStyle {
		path += c.cfg.Bucket + "/"
	} else {
		u.Host = c.cfg.Bucket + "." + u.Host
	}
	path += key
	u.Path = path
	u.RawPath = uriEncode(path, false)
	u.RawQuery = canonicalQuery(q)
	return &u, u.RawPath
}

// uriEncode encodes per SigV4: everything but unreserved characters (and '/'
// in paths).
func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_' || ch == '.' || ch == '~' || (ch == '/' && !encodeSlash) {
			b.WriteByte(ch)
		} else {
			fmt.Fprintf(&b, "%%%02X", ch)
		}
	}
	return b.String()
}

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

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// sign adds SigV4 headers to req; payloadHash is the hex sha256 of the body.
func (c *Client) sign(req *http.Request, canonicalPath, payloadHash string) {
	t := c.now().UTC()
	amzDate := t.Format("20060102T150405Z")
	date := t.Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	headers := map[string]string{"host": req.URL.Host}
	for k, v := range req.Header {
		lk := strings.ToLower(k)
		if lk == "content-type" || lk == "content-md5" || strings.HasPrefix(lk, "x-amz-") {
			headers[lk] = strings.TrimSpace(strings.Join(v, ","))
		}
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var ch strings.Builder
	for _, k := range names {
		ch.WriteString(k + ":" + headers[k] + "\n")
	}
	signed := strings.Join(names, ";")
	creq := strings.Join([]string{req.Method, canonicalPath, req.URL.RawQuery, ch.String(), signed, payloadHash}, "\n")
	scope := date + "/" + c.cfg.Region + "/s3/aws4_request"
	sum := sha256.Sum256([]byte(creq))
	sts := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	k := hmacSHA256([]byte("AWS4"+c.cfg.SecretKey), date)
	k = hmacSHA256(k, c.cfg.Region)
	k = hmacSHA256(k, "s3")
	k = hmacSHA256(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(k, sts))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+c.cfg.AccessKey+"/"+scope+", SignedHeaders="+signed+", Signature="+sig)
}

// do sends a signed request. body may be nil; it is re-created per attempt.
func (c *Client) do(ctx context.Context, method, key string, q url.Values, body func() io.Reader, size int64, payloadHash string, hdr http.Header) (*http.Response, error) {
	if payloadHash == "" {
		payloadHash = emptySHA256
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * c.RetryWait):
			}
		}
		u, cpath := c.objectURL(key, q)
		var r io.Reader
		if body != nil {
			r = body()
		}
		req, err := http.NewRequestWithContext(ctx, method, u.String(), r)
		if err != nil {
			return nil, err
		}
		req.URL.RawPath, req.URL.RawQuery = cpath, u.RawQuery
		if body != nil {
			req.ContentLength = size
		}
		for k, v := range hdr {
			req.Header[k] = v
		}
		c.sign(req, cpath, payloadHash)
		res, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return nil, err
			}
			continue
		}
		if res.StatusCode >= 500 || res.StatusCode == http.StatusTooManyRequests {
			lastErr = readError(res)
			continue
		}
		if res.StatusCode >= 300 {
			return nil, readError(res)
		}
		return res, nil
	}
	return nil, lastErr
}

func readError(res *http.Response) error {
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	var e struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	_ = xml.Unmarshal(b, &e)
	if e.Code == "" && res.StatusCode == http.StatusNotFound {
		e.Code = "NotFound"
	}
	return &Error{Status: res.StatusCode, Code: e.Code, Message: e.Message}
}

func sha256Hex(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// PutBytes stores small content.
func (c *Client) PutBytes(ctx context.Context, key string, data []byte) error {
	s := sha256.Sum256(data)
	res, err := c.do(ctx, http.MethodPut, c.FullKey(key), nil, func() io.Reader { return bytes.NewReader(data) }, int64(len(data)), hex.EncodeToString(s[:]), nil)
	if err != nil {
		return err
	}
	_ = res.Body.Close()
	return nil
}

// Upload stores size bytes from r, using multipart uploads above PartSize.
// Parts are read straight from r (twice: once to hash, once to send), so
// memory use doesn't grow with the file.
func (c *Client) Upload(ctx context.Context, key string, r io.ReaderAt, size int64) error {
	key = c.FullKey(key)
	part := c.PartSize
	if part < 5<<20 {
		part = 5 << 20
	}
	for size/part >= 10000 { // S3 allows 10,000 parts
		part *= 2
	}
	if size <= part {
		return c.putSection(ctx, key, nil, io.NewSectionReader(r, 0, size), size)
	}

	res, err := c.do(ctx, http.MethodPost, key, url.Values{"uploads": {""}}, nil, 0, "", nil)
	if err != nil {
		return fmt.Errorf("start multipart upload: %w", err)
	}
	var init struct {
		UploadID string `xml:"UploadId"`
	}
	err = xml.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&init)
	_ = res.Body.Close()
	if err != nil || init.UploadID == "" {
		return fmt.Errorf("start multipart upload: bad response: %w", err)
	}
	abort := func() {
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if res, err := c.do(actx, http.MethodDelete, key, url.Values{"uploadId": {init.UploadID}}, nil, 0, "", nil); err == nil {
			_ = res.Body.Close()
		}
	}

	type done struct {
		XMLName xml.Name `xml:"Part"`
		Number  int      `xml:"PartNumber"`
		ETag    string   `xml:"ETag"`
	}
	var parts []done
	for n, off := 1, int64(0); off < size; n, off = n+1, off+part {
		l := part
		if off+l > size {
			l = size - off
		}
		q := url.Values{"partNumber": {strconv.Itoa(n)}, "uploadId": {init.UploadID}}
		etag, err := c.putPart(ctx, key, q, io.NewSectionReader(r, off, l), l)
		if err != nil {
			abort()
			return fmt.Errorf("upload part %d: %w", n, err)
		}
		parts = append(parts, done{Number: n, ETag: etag})
	}
	body, _ := xml.Marshal(struct {
		XMLName xml.Name `xml:"CompleteMultipartUpload"`
		Parts   []done
	}{Parts: parts})
	s := sha256.Sum256(body)
	res, err = c.do(ctx, http.MethodPost, key, url.Values{"uploadId": {init.UploadID}}, func() io.Reader { return bytes.NewReader(body) },
		int64(len(body)), hex.EncodeToString(s[:]), http.Header{"Content-Type": {"application/xml"}})
	if err != nil {
		abort()
		return fmt.Errorf("complete multipart upload: %w", err)
	}
	// Some providers report a failure inside a 200 response.
	b, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	_ = res.Body.Close()
	if bytes.Contains(b, []byte("<Error>")) {
		abort()
		var e struct {
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
		}
		_ = xml.Unmarshal(b, &e)
		return &Error{Status: 200, Code: e.Code, Message: e.Message}
	}
	return nil
}

func (c *Client) putSection(ctx context.Context, key string, q url.Values, sr *io.SectionReader, size int64) error {
	res, err := c.sendSection(ctx, key, q, sr, size)
	if err != nil {
		return err
	}
	_ = res.Body.Close()
	return nil
}

func (c *Client) putPart(ctx context.Context, key string, q url.Values, sr *io.SectionReader, size int64) (string, error) {
	res, err := c.sendSection(ctx, key, q, sr, size)
	if err != nil {
		return "", err
	}
	_ = res.Body.Close()
	etag := res.Header.Get("ETag")
	if etag == "" {
		return "", errors.New("no ETag in the response")
	}
	return etag, nil
}

func (c *Client) sendSection(ctx context.Context, key string, q url.Values, sr *io.SectionReader, size int64) (*http.Response, error) {
	hash, err := sha256Hex(io.NewSectionReader(sr, 0, size))
	if err != nil {
		return nil, err
	}
	return c.do(ctx, http.MethodPut, key, q, func() io.Reader { return io.NewSectionReader(sr, 0, size) }, size, hash, nil)
}

// Get opens an object; the caller closes the body.
func (c *Client) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	res, err := c.do(ctx, http.MethodGet, c.FullKey(key), nil, nil, 0, "", nil)
	if err != nil {
		return nil, 0, err
	}
	return res.Body, res.ContentLength, nil
}

// Delete removes an object (missing objects are not an error).
func (c *Client) Delete(ctx context.Context, key string) error {
	res, err := c.do(ctx, http.MethodDelete, c.FullKey(key), nil, nil, 0, "", nil)
	if err != nil {
		if IsNotFound(err) {
			return nil
		}
		return err
	}
	_ = res.Body.Close()
	return nil
}

// List returns the objects under prefix (relative to the configured prefix).
func (c *Client) List(ctx context.Context, prefix string) ([]Object, error) {
	full := c.FullKey(prefix)
	var out []Object
	token := ""
	for page := 0; page < 1000; page++ {
		q := url.Values{"list-type": {"2"}, "prefix": {full}, "max-keys": {"1000"}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		res, err := c.do(ctx, http.MethodGet, "", q, nil, 0, "", nil)
		if err != nil {
			return nil, err
		}
		var body struct {
			Contents []struct {
				Key          string    `xml:"Key"`
				Size         int64     `xml:"Size"`
				LastModified time.Time `xml:"LastModified"`
			} `xml:"Contents"`
			IsTruncated           bool   `xml:"IsTruncated"`
			NextContinuationToken string `xml:"NextContinuationToken"`
		}
		err = xml.NewDecoder(io.LimitReader(res.Body, 32<<20)).Decode(&body)
		_ = res.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("list objects: %w", err)
		}
		for _, o := range body.Contents {
			rel := o.Key
			if c.cfg.Prefix != "" {
				rel = strings.TrimPrefix(rel, c.cfg.Prefix+"/")
			}
			out = append(out, Object{Key: rel, Size: o.Size, LastModified: o.LastModified})
		}
		if !body.IsTruncated || body.NextContinuationToken == "" {
			return out, nil
		}
		token = body.NextContinuationToken
	}
	return out, nil
}

// Test checks that the bucket exists (without creating it, which some
// services do on first write), then writes, reads back and deletes a probe.
func (c *Client) Test(ctx context.Context) error {
	res, err := c.do(ctx, http.MethodHead, "", nil, nil, 0, "", nil)
	if err != nil {
		var e *Error
		if errors.As(err, &e) && (e.Code == "NoSuchBucket" || e.Status == http.StatusNotFound) {
			return fmt.Errorf("bucket %q does not exist", c.cfg.Bucket)
		}
		if errors.As(err, &e) && e.Status == http.StatusForbidden {
			return errors.New("access denied: check the access key, secret key and region, and that the key may use this bucket")
		}
		if errors.As(err, &e) && e.Status == http.StatusMovedPermanently {
			return errors.New("the bucket is in another region: set the bucket's region")
		}
		return fmt.Errorf("check the bucket: %w", err)
	}
	_ = res.Body.Close()

	var rnd [8]byte
	_, _ = rand.Read(rnd[:])
	key := ".docklet-probe-" + hex.EncodeToString(rnd[:])
	data := []byte("docklet connection test " + c.now().UTC().Format(time.RFC3339))
	if err := c.PutBytes(ctx, key, data); err != nil {
		return fmt.Errorf("write a test object: %w", err)
	}
	body, _, err := c.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("read the test object back: %w", err)
	}
	got, err := io.ReadAll(io.LimitReader(body, 1<<20))
	_ = body.Close()
	if err != nil || !bytes.Equal(got, data) {
		return errors.New("read the test object back: content differs")
	}
	if err := c.Delete(ctx, key); err != nil {
		return fmt.Errorf("delete the test object: %w", err)
	}
	return nil
}
