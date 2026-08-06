package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/blob"
)

// fakeS3 is enough of the S3 REST API to drive the driver: single PUT,
// multipart, GET with Range, HEAD, and DELETE.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	parts   map[string]map[int][]byte
	// completeReturns200Error reproduces S3 reporting a failure inside a 200.
	completeReturns200Error bool
	lastAuth                string
	aborted                 bool
}

func newFakeS3() *fakeS3 {
	return &fakeS3{objects: map[string][]byte{}, parts: map[string]map[int][]byte{}}
}

func (f *fakeS3) key(r *http.Request) string {
	return strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/"), "bucket/")
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastAuth = r.Header.Get("Authorization")
	key := f.key(r)
	q := r.URL.Query()

	switch r.Method {
	case http.MethodPost:
		if _, ok := q["uploads"]; ok {
			f.parts["u1"] = map[int][]byte{}
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprint(w, `<?xml version="1.0"?><InitiateMultipartUploadResult><UploadId>u1</UploadId></InitiateMultipartUploadResult>`)
			return
		}
		if id := q.Get("uploadId"); id != "" {
			body, _ := io.ReadAll(r.Body)
			var complete completeUpload
			if err := xml.Unmarshal(body, &complete); err != nil {
				http.Error(w, "bad xml", http.StatusBadRequest)
				return
			}
			if f.completeReturns200Error {
				fmt.Fprint(w, `<?xml version="1.0"?><Error><Code>InternalError</Code><Message>we lost it</Message></Error>`)
				return
			}
			var assembled []byte
			for _, p := range complete.Parts {
				assembled = append(assembled, f.parts[id][p.PartNumber]...)
			}
			f.objects[key] = assembled
			fmt.Fprint(w, `<?xml version="1.0"?><CompleteMultipartUploadResult><ETag>"etag"</ETag></CompleteMultipartUploadResult>`)
			return
		}
		http.Error(w, "unexpected POST", http.StatusBadRequest)

	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		if id := q.Get("uploadId"); id != "" {
			n, _ := strconv.Atoi(q.Get("partNumber"))
			f.parts[id][n] = body
			w.Header().Set("ETag", fmt.Sprintf("%q", n))
			w.WriteHeader(http.StatusOK)
			return
		}
		f.objects[key] = body
		w.WriteHeader(http.StatusOK)

	case http.MethodGet:
		obj, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `<Error><Code>NoSuchKey</Code><Message>gone</Message></Error>`)
			return
		}
		if rng := r.Header.Get("Range"); rng != "" {
			var start, end int64
			if n, _ := fmt.Sscanf(rng, "bytes=%d-%d", &start, &end); n == 2 {
				if end >= int64(len(obj)) {
					end = int64(len(obj)) - 1
				}
				w.WriteHeader(http.StatusPartialContent)
				w.Write(obj[start : end+1])
				return
			}
			fmt.Sscanf(rng, "bytes=%d-", &start)
			w.WriteHeader(http.StatusPartialContent)
			w.Write(obj[start:])
			return
		}
		w.Write(obj)

	case http.MethodHead:
		obj, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(obj)))
		w.Header().Set("Last-Modified", "Fri, 24 May 2013 00:00:00 GMT")
		w.WriteHeader(http.StatusOK)

	case http.MethodDelete:
		if q.Get("uploadId") != "" {
			f.aborted = true
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if _, ok := f.objects[key]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	}
}

func newTestStore(t *testing.T, f *fakeS3, mutate func(*Config)) (*Store, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	cfg := Config{
		Endpoint:        srv.URL,
		Region:          "us-east-1",
		Bucket:          "bucket",
		AccessKeyID:     "AK",
		SecretAccessKey: "SK",
		UsePathStyle:    true,
		HTTPClient:      srv.Client(),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	s, err := New(cfg)
	require.NoError(t, err)
	return s, srv
}

func TestNewValidatesConfig(t *testing.T) {
	_, err := New(Config{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Endpoint, Region, Bucket, AccessKeyID, SecretAccessKey")

	_, err = New(Config{Endpoint: "not a url at all", Region: "r", Bucket: "b", AccessKeyID: "a", SecretAccessKey: "s"})
	require.Error(t, err)

	_, err = New(Config{Endpoint: "https://x", Region: "r", Bucket: "b", AccessKeyID: "a", SecretAccessKey: "s", PartSize: 1024})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "minimum")

	s, err := New(Config{Endpoint: "https://x", Region: "r", Bucket: "b", AccessKeyID: "a", SecretAccessKey: "s"})
	require.NoError(t, err)
	assert.Equal(t, int64(DefaultPartSize), s.cfg.PartSize)
}

func TestPutSingleAndGet(t *testing.T) {
	ctx := context.Background()
	f := newFakeS3()
	s, _ := newTestStore(t, f, nil)

	body := []byte("small object")
	n, digest, err := s.Put(ctx, "runs/1/a.zip", bytes.NewReader(body))
	require.NoError(t, err)
	assert.Equal(t, int64(len(body)), n)
	want := sha256.Sum256(body)
	assert.Equal(t, hex.EncodeToString(want[:]), digest)
	assert.Contains(t, f.lastAuth, "AWS4-HMAC-SHA256 Credential=AK/")

	rc, err := s.Get(ctx, "runs/1/a.zip")
	require.NoError(t, err)
	defer rc.Close()
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, body, got)
}

func TestPutMultipart(t *testing.T) {
	ctx := context.Background()
	f := newFakeS3()
	s, _ := newTestStore(t, f, func(c *Config) {
		c.PartSize = MinPartSize
		c.MultipartThreshold = MinPartSize
	})

	body := bytes.Repeat([]byte("x"), MinPartSize*2+17)
	n, digest, err := s.Put(ctx, "big", bytes.NewReader(body))
	require.NoError(t, err)
	assert.Equal(t, int64(len(body)), n)
	want := sha256.Sum256(body)
	assert.Equal(t, hex.EncodeToString(want[:]), digest)
	assert.Equal(t, body, f.objects["big"], "parts must reassemble in order")
	assert.Len(t, f.parts["u1"], 3)
}

func TestCompleteMultipartErrorInsideTwoHundredIsNotSuccess(t *testing.T) {
	ctx := context.Background()
	f := newFakeS3()
	f.completeReturns200Error = true
	s, _ := newTestStore(t, f, func(c *Config) {
		c.PartSize = MinPartSize
		c.MultipartThreshold = MinPartSize
	})

	_, _, err := s.Put(ctx, "big", bytes.NewReader(bytes.Repeat([]byte("x"), MinPartSize+1)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "inside a 200 response")
	assert.Contains(t, err.Error(), "we lost it")
	assert.True(t, f.aborted, "a failed complete must abort the upload")
}

func TestGetRangeAndStatAndDelete(t *testing.T) {
	ctx := context.Background()
	f := newFakeS3()
	s, _ := newTestStore(t, f, nil)
	_, _, err := s.Put(ctx, "k", strings.NewReader("0123456789"))
	require.NoError(t, err)

	rc, err := s.GetRange(ctx, "k", 2, 3)
	require.NoError(t, err)
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	rc.Close()
	assert.Equal(t, "234", string(got))

	rc, err = s.GetRange(ctx, "k", 7, -1)
	require.NoError(t, err)
	got, err = io.ReadAll(rc)
	require.NoError(t, err)
	rc.Close()
	assert.Equal(t, "789", string(got))

	info, err := s.Stat(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, int64(10), info.Size)
	assert.Equal(t, 2013, info.ModTime.Year())

	require.NoError(t, s.Delete(ctx, "k"))
	_, err = s.Stat(ctx, "k")
	assert.ErrorIs(t, err, blob.ErrNotFound)
}

func TestMissingKeyIsNotFoundWithS3Reason(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t, newFakeS3(), nil)

	_, err := s.Get(ctx, "nope")
	assert.ErrorIs(t, err, blob.ErrNotFound)
	assert.ErrorIs(t, s.Delete(ctx, "nope"), blob.ErrNotFound)
	_, err = s.GetRange(ctx, "nope", 0, 1)
	assert.ErrorIs(t, err, blob.ErrNotFound)
}

func TestErrorBodyIsSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `<Error><Code>AccessDenied</Code><Message>no permission</Message></Error>`)
	}))
	defer srv.Close()
	s, err := New(Config{
		Endpoint: srv.URL, Region: "us-east-1", Bucket: "b",
		AccessKeyID: "AK", SecretAccessKey: "SK", UsePathStyle: true, HTTPClient: srv.Client(),
	})
	require.NoError(t, err)

	_, _, err = s.Put(context.Background(), "k", strings.NewReader("x"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "AccessDenied")
	assert.Contains(t, err.Error(), "no permission")
}

func TestPutAtIsUnsupported(t *testing.T) {
	s, _ := newTestStore(t, newFakeS3(), nil)
	_, err := s.PutAt(context.Background(), "k", 0, strings.NewReader("x"))
	assert.ErrorIs(t, err, blob.ErrUnsupported)
	assert.Contains(t, err.Error(), "blob.ChunkedUpload")
}

func TestBadKeysAreRejectedBeforeAnyRequest(t *testing.T) {
	ctx := context.Background()
	f := newFakeS3()
	s, _ := newTestStore(t, f, nil)
	for _, key := range []string{"../escape", "/absolute", "a\x00b"} {
		_, _, err := s.Put(ctx, key, strings.NewReader("x"))
		assert.ErrorIs(t, err, blob.ErrBadKey)
		_, err = s.Get(ctx, key)
		assert.ErrorIs(t, err, blob.ErrBadKey)
		_, err = s.Stat(ctx, key)
		assert.ErrorIs(t, err, blob.ErrBadKey)
		assert.ErrorIs(t, s.Delete(ctx, key), blob.ErrBadKey)
		_, err = s.SignedURL(ctx, key, time.Minute)
		assert.ErrorIs(t, err, blob.ErrBadKey)
	}
	assert.Empty(t, f.objects)
}

func TestSignedURL(t *testing.T) {
	fixed := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	s, err := New(Config{
		Endpoint: "https://s3.us-east-1.amazonaws.com", Region: "us-east-1", Bucket: "examplebucket",
		AccessKeyID: "AKIAIOSFODNN7EXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		Now: func() time.Time { return fixed },
	})
	require.NoError(t, err)

	raw, err := s.SignedURL(context.Background(), "test.txt", 24*time.Hour)
	require.NoError(t, err)
	u, err := url.Parse(raw)
	require.NoError(t, err)
	assert.Equal(t, "examplebucket.s3.us-east-1.amazonaws.com", u.Host)
	assert.Equal(t, "86400", u.Query().Get("X-Amz-Expires"))
	assert.NotEmpty(t, u.Query().Get("X-Amz-Signature"))

	_, err = s.SignedURL(context.Background(), "test.txt", 0)
	assert.Error(t, err, "a non-positive ttl is a caller bug, not a default")
}

func TestVirtualHostAddressing(t *testing.T) {
	s, err := New(Config{
		Endpoint: "https://s3.example.com", Region: "r", Bucket: "b",
		AccessKeyID: "a", SecretAccessKey: "s",
	})
	require.NoError(t, err)
	u, err := s.objectURL("x/y.zip")
	require.NoError(t, err)
	assert.Equal(t, "https://b.s3.example.com/x/y.zip", u.String())
}
