// Package s3test is an in-memory S3 server for tests (path-style, no
// signature checks): HEAD/PUT/GET/DELETE objects, ListObjectsV2 and
// multipart uploads.
package s3test

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"time"
)

// Server is a fake S3 endpoint.
type Server struct {
	*httptest.Server
	mu       sync.Mutex
	buckets  map[string]map[string][]byte
	uploads  map[string]map[int][]byte
	seq      int
	failPuts bool
}

// FailPuts makes object writes fail with 500 (simulates an outage).
func (s *Server) FailPuts(fail bool) {
	s.mu.Lock()
	s.failPuts = fail
	s.mu.Unlock()
}

// New starts a server with the given buckets.
func New(buckets ...string) *Server {
	s := &Server{buckets: map[string]map[string][]byte{}, uploads: map[string]map[int][]byte{}}
	for _, b := range buckets {
		s.buckets[b] = map[string][]byte{}
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// Keys lists the objects of a bucket under prefix.
func (s *Server) Keys(bucket, prefix string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.buckets[bucket] {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Object returns an object's content.
func (s *Server) Object(bucket, key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.buckets[bucket][key]
	return b, ok
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
		http.Error(w, "<Error><Code>AccessDenied</Code></Error>", http.StatusForbidden)
		return
	}
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	s.mu.Lock()
	defer s.mu.Unlock()
	objs, ok := s.buckets[bucket]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "<Error><Code>NoSuchBucket</Code><Message>no such bucket</Message></Error>")
		return
	}
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodHead && key == "":
	case r.Method == http.MethodGet && key == "" && q.Get("list-type") == "2":
		type content struct {
			Key          string
			Size         int
			LastModified string
		}
		var res struct {
			XMLName  xml.Name `xml:"ListBucketResult"`
			Contents []content
		}
		keys := make([]string, 0, len(objs))
		for k := range objs {
			if strings.HasPrefix(k, q.Get("prefix")) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			res.Contents = append(res.Contents, content{Key: k, Size: len(objs[k]), LastModified: time.Now().UTC().Format(time.RFC3339)})
		}
		b, _ := xml.Marshal(res)
		_, _ = w.Write(b)
	case r.Method == http.MethodPost && q.Has("uploads"):
		s.seq++
		id := fmt.Sprintf("u%d", s.seq)
		s.uploads[id] = map[int][]byte{}
		fmt.Fprintf(w, "<InitiateMultipartUploadResult><UploadId>%s</UploadId></InitiateMultipartUploadResult>", id)
	case r.Method == http.MethodPut && q.Get("uploadId") != "":
		var n int
		_, _ = fmt.Sscan(q.Get("partNumber"), &n)
		b, _ := io.ReadAll(r.Body)
		s.uploads[q.Get("uploadId")][n] = b
		w.Header().Set("ETag", fmt.Sprintf(`"p%d"`, n))
	case r.Method == http.MethodPost && q.Get("uploadId") != "":
		parts := s.uploads[q.Get("uploadId")]
		var data []byte
		for i := 1; i <= len(parts); i++ {
			data = append(data, parts[i]...)
		}
		objs[key] = data
		delete(s.uploads, q.Get("uploadId"))
		_, _ = io.WriteString(w, "<CompleteMultipartUploadResult/>")
	case r.Method == http.MethodDelete && q.Get("uploadId") != "":
		delete(s.uploads, q.Get("uploadId"))
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut:
		if s.failPuts {
			http.Error(w, "<Error><Code>InternalError</Code></Error>", http.StatusInternalServerError)
			return
		}
		b, _ := io.ReadAll(r.Body)
		objs[key] = b
	case r.Method == http.MethodGet:
		b, ok := objs[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, "<Error><Code>NoSuchKey</Code></Error>")
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(b)))
		_, _ = w.Write(b)
	case r.Method == http.MethodDelete:
		delete(objs, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "unsupported", http.StatusBadRequest)
	}
}
