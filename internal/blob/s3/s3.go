// Package s3 is the S3-compatible blob driver, written against net/http with
// hand-rolled SigV4 signing (see sigv4.go) so the platform does not take on the
// AWS SDK's dependency tree to sign four verbs.
//
// It targets the S3 REST API, so MinIO, Ceph RGW, R2, and Backblaze B2's S3
// endpoint all work; nothing here is AWS-specific beyond the signing algorithm.
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
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/blob"
)

// MinPartSize is S3's floor for every part but the last.
const MinPartSize = 5 << 20

// DefaultPartSize matches the 8 MiB chunk actions/upload-artifact streams.
const DefaultPartSize = 8 << 20

// Config describes one bucket. Every field without a documented default is
// required; New reports which one is missing rather than failing at first use.
type Config struct {
	// Endpoint is the service root, e.g. https://s3.us-east-1.amazonaws.com or
	// http://minio:9000.
	Endpoint        string
	Region          string
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	// UsePathStyle addresses the bucket as {endpoint}/{bucket}/{key}, which is
	// what MinIO and most non-AWS implementations want.
	UsePathStyle bool
	HTTPClient   *http.Client
	// PartSize is the multipart part size; defaults to DefaultPartSize.
	PartSize int64
	// MultipartThreshold is the size above which an upload goes multipart;
	// defaults to PartSize.
	MultipartThreshold int64
	// Now exists so tests can pin the signing time.
	Now func() time.Time
}

// Store is a blob.Store backed by an S3-compatible bucket.
type Store struct {
	cfg      Config
	endpoint *url.URL
	client   *http.Client
	creds    credentials
	now      func() time.Time
}

var _ blob.Store = (*Store)(nil)

// New validates cfg and returns a driver. A missing credential is a startup
// failure: an unauthenticated bucket client would fail every upload later with
// a much worse error.
func New(cfg Config) (*Store, error) {
	required := []struct{ name, value string }{
		{"Endpoint", cfg.Endpoint},
		{"Region", cfg.Region},
		{"Bucket", cfg.Bucket},
		{"AccessKeyID", cfg.AccessKeyID},
		{"SecretAccessKey", cfg.SecretAccessKey},
	}
	var missing []string
	for _, f := range required {
		if f.value == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("blob/s3: config is incomplete: %s must be set", strings.Join(missing, ", "))
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("blob/s3: endpoint %q: %w", cfg.Endpoint, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("blob/s3: endpoint %q must be an absolute URL", cfg.Endpoint)
	}
	if cfg.PartSize == 0 {
		cfg.PartSize = DefaultPartSize
	}
	if cfg.PartSize < MinPartSize {
		return nil, fmt.Errorf("blob/s3: PartSize %d is below S3's %d byte minimum", cfg.PartSize, MinPartSize)
	}
	if cfg.MultipartThreshold == 0 {
		cfg.MultipartThreshold = cfg.PartSize
	}
	s := &Store{
		cfg:      cfg,
		endpoint: u,
		client:   cfg.HTTPClient,
		creds:    credentials{cfg.AccessKeyID, cfg.SecretAccessKey, cfg.SessionToken},
		now:      cfg.Now,
	}
	if s.client == nil {
		s.client = &http.Client{Timeout: 5 * time.Minute}
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s, nil
}

// objectURL builds the request URL for a key.
func (s *Store) objectURL(key string) (*url.URL, error) {
	if err := blob.ValidateKey(key); err != nil {
		return nil, err
	}
	u := *s.endpoint
	if s.cfg.UsePathStyle {
		u.Path = strings.TrimSuffix(u.Path, "/") + "/" + s.cfg.Bucket + "/" + key
	} else {
		u.Host = s.cfg.Bucket + "." + u.Host
		u.Path = strings.TrimSuffix(u.Path, "/") + "/" + key
	}
	return &u, nil
}

// do signs and sends a request. body must be rewindable or nil.
func (s *Store) do(ctx context.Context, method string, u *url.URL, body []byte, payloadHash string, headers map[string]string) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = int64(len(body))
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if payloadHash == "" {
		payloadHash = EmptyPayloadHash
	}
	signRequest(req, s.creds, s.cfg.Region, "s3", payloadHash, s.now())
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("blob/s3: %s %s: %w", method, u.Path, err)
	}
	return resp, nil
}

// s3Error is the XML body S3 returns for a failure.
type s3Error struct {
	XMLName xml.Name `xml:"Error"`
	Code    string   `xml:"Code"`
	Message string   `xml:"Message"`
}

// errorFrom turns a non-2xx response into a named error, reading the XML body
// so the operator sees S3's own reason rather than a bare status code.
func errorFrom(op, key string, resp *http.Response) error {
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %s", blob.ErrNotFound, key)
	}
	var e s3Error
	if err := xml.Unmarshal(body, &e); err == nil && e.Code != "" {
		return fmt.Errorf("blob/s3: %s %s: %s (%s): %s", op, key, resp.Status, e.Code, e.Message)
	}
	return fmt.Errorf("blob/s3: %s %s: %s: %s", op, key, resp.Status, strings.TrimSpace(string(body)))
}

func ok(resp *http.Response) bool {
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// Put stores the whole object, switching to multipart above the configured
// threshold. It returns the size and hex sha256 of what was actually sent.
func (s *Store) Put(ctx context.Context, key string, r io.Reader) (int64, string, error) {
	head := make([]byte, s.cfg.MultipartThreshold+1)
	n, err := io.ReadFull(r, head)
	switch {
	case err == io.EOF || err == io.ErrUnexpectedEOF:
		return s.putSingle(ctx, key, head[:n])
	case err != nil:
		return 0, "", fmt.Errorf("blob/s3: read body for %s: %w", key, err)
	}
	return s.putMultipart(ctx, key, io.MultiReader(bytes.NewReader(head[:n]), r))
}

func (s *Store) putSingle(ctx context.Context, key string, body []byte) (int64, string, error) {
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	u, err := s.objectURL(key)
	if err != nil {
		return 0, "", err
	}
	resp, err := s.do(ctx, http.MethodPut, u, body, digest, map[string]string{
		"Content-Type": "application/octet-stream",
	})
	if err != nil {
		return 0, "", err
	}
	if !ok(resp) {
		return 0, "", errorFrom("put", key, resp)
	}
	resp.Body.Close()
	return int64(len(body)), digest, nil
}

type initiateResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	UploadID string   `xml:"UploadId"`
}

type completePart struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}

type completeUpload struct {
	XMLName xml.Name       `xml:"CompleteMultipartUpload"`
	Parts   []completePart `xml:"Part"`
}

func (s *Store) putMultipart(ctx context.Context, key string, r io.Reader) (int64, string, error) {
	u, err := s.objectURL(key)
	if err != nil {
		return 0, "", err
	}
	initURL := *u
	initURL.RawQuery = "uploads="
	resp, err := s.do(ctx, http.MethodPost, &initURL, []byte{}, EmptyPayloadHash, nil)
	if err != nil {
		return 0, "", err
	}
	if !ok(resp) {
		return 0, "", errorFrom("create multipart upload", key, resp)
	}
	var init initiateResult
	dec := xml.NewDecoder(resp.Body)
	decErr := dec.Decode(&init)
	resp.Body.Close()
	if decErr != nil || init.UploadID == "" {
		return 0, "", fmt.Errorf("blob/s3: create multipart upload %s: response carried no UploadId: %v", key, decErr)
	}

	size, digest, parts, err := s.uploadParts(ctx, key, u, init.UploadID, r)
	if err != nil {
		s.abortMultipart(ctx, u, init.UploadID)
		return 0, "", err
	}
	if err := s.completeMultipart(ctx, key, u, init.UploadID, parts); err != nil {
		s.abortMultipart(ctx, u, init.UploadID)
		return 0, "", err
	}
	return size, digest, nil
}

func (s *Store) uploadParts(ctx context.Context, key string, u *url.URL, uploadID string, r io.Reader) (int64, string, []completePart, error) {
	h := sha256.New()
	buf := make([]byte, s.cfg.PartSize)
	var total int64
	var parts []completePart
	for num := 1; ; num++ {
		n, err := io.ReadFull(r, buf)
		if n == 0 {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			if err != nil {
				return 0, "", nil, fmt.Errorf("blob/s3: read part %d of %s: %w", num, key, err)
			}
			break
		}
		chunk := buf[:n]
		h.Write(chunk)
		total += int64(n)

		sum := sha256.Sum256(chunk)
		partURL := *u
		q := url.Values{"partNumber": {strconv.Itoa(num)}, "uploadId": {uploadID}}
		partURL.RawQuery = q.Encode()
		resp, derr := s.do(ctx, http.MethodPut, &partURL, chunk, hex.EncodeToString(sum[:]), nil)
		if derr != nil {
			return 0, "", nil, derr
		}
		if !ok(resp) {
			return 0, "", nil, errorFrom(fmt.Sprintf("upload part %d of", num), key, resp)
		}
		etag := resp.Header.Get("ETag")
		resp.Body.Close()
		if etag == "" {
			return 0, "", nil, fmt.Errorf("blob/s3: upload part %d of %s: response carried no ETag", num, key)
		}
		parts = append(parts, completePart{PartNumber: num, ETag: etag})

		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
	}
	if len(parts) == 0 {
		return 0, "", nil, fmt.Errorf("blob/s3: multipart upload of %s had no parts", key)
	}
	return total, hex.EncodeToString(h.Sum(nil)), parts, nil
}

func (s *Store) completeMultipart(ctx context.Context, key string, u *url.URL, uploadID string, parts []completePart) error {
	body, err := xml.Marshal(completeUpload{Parts: parts})
	if err != nil {
		return fmt.Errorf("blob/s3: encode complete-multipart body for %s: %w", key, err)
	}
	sum := sha256.Sum256(body)
	doneURL := *u
	doneURL.RawQuery = url.Values{"uploadId": {uploadID}}.Encode()
	resp, err := s.do(ctx, http.MethodPost, &doneURL, body, hex.EncodeToString(sum[:]), map[string]string{
		"Content-Type": "application/xml",
	})
	if err != nil {
		return err
	}
	if !ok(resp) {
		return errorFrom("complete multipart upload", key, resp)
	}
	// S3 can report a failure inside a 200 body on this call, so the body is
	// inspected rather than trusted.
	payload, rerr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if rerr != nil {
		return fmt.Errorf("blob/s3: complete multipart upload %s: read response: %w", key, rerr)
	}
	if bytes.Contains(payload, []byte("<Error")) {
		var e s3Error
		_ = xml.Unmarshal(payload, &e)
		return fmt.Errorf("blob/s3: complete multipart upload %s failed inside a 200 response: %s: %s", key, e.Code, e.Message)
	}
	return nil
}

func (s *Store) abortMultipart(ctx context.Context, u *url.URL, uploadID string) {
	abortURL := *u
	abortURL.RawQuery = url.Values{"uploadId": {uploadID}}.Encode()
	resp, err := s.do(ctx, http.MethodDelete, &abortURL, nil, EmptyPayloadHash, nil)
	if err == nil {
		resp.Body.Close()
	}
}

// PutAt is not implementable on S3: the API has no ranged write. Callers use
// blob.ChunkedUpload, which stages chunks and assembles them.
func (s *Store) PutAt(ctx context.Context, key string, offset int64, r io.Reader) (int64, error) {
	return 0, fmt.Errorf("%w: S3 has no ranged write; stage chunks with blob.ChunkedUpload", blob.ErrUnsupported)
}

// Get streams the whole object.
func (s *Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return s.get(ctx, key, "")
}

// GetRange streams length bytes from off; a negative length reads to the end.
func (s *Store) GetRange(ctx context.Context, key string, off, length int64) (io.ReadCloser, error) {
	if off < 0 {
		return nil, fmt.Errorf("blob/s3: negative range offset %d for %s", off, key)
	}
	rng := fmt.Sprintf("bytes=%d-", off)
	if length >= 0 {
		rng = fmt.Sprintf("bytes=%d-%d", off, off+length-1)
	}
	return s.get(ctx, key, rng)
}

func (s *Store) get(ctx context.Context, key, rng string) (io.ReadCloser, error) {
	u, err := s.objectURL(key)
	if err != nil {
		return nil, err
	}
	headers := map[string]string{}
	if rng != "" {
		headers["Range"] = rng
	}
	resp, err := s.do(ctx, http.MethodGet, u, nil, EmptyPayloadHash, headers)
	if err != nil {
		return nil, err
	}
	if !ok(resp) {
		return nil, errorFrom("get", key, resp)
	}
	return resp.Body, nil
}

// Stat reports the object's size and modification time.
func (s *Store) Stat(ctx context.Context, key string) (blob.Info, error) {
	u, err := s.objectURL(key)
	if err != nil {
		return blob.Info{}, err
	}
	resp, err := s.do(ctx, http.MethodHead, u, nil, EmptyPayloadHash, nil)
	if err != nil {
		return blob.Info{}, err
	}
	defer resp.Body.Close()
	if !ok(resp) {
		if resp.StatusCode == http.StatusNotFound {
			return blob.Info{}, fmt.Errorf("%w: %s", blob.ErrNotFound, key)
		}
		return blob.Info{}, fmt.Errorf("blob/s3: head %s: %s", key, resp.Status)
	}
	info := blob.Info{Key: key, Size: resp.ContentLength}
	if cl := resp.Header.Get("Content-Length"); cl != "" && info.Size <= 0 {
		if v, cerr := strconv.ParseInt(cl, 10, 64); cerr == nil {
			info.Size = v
		}
	}
	if lm := resp.Header.Get("Last-Modified"); lm != "" {
		if t, terr := http.ParseTime(lm); terr == nil {
			info.ModTime = t
		}
	}
	return info, nil
}

// Delete removes the object.
func (s *Store) Delete(ctx context.Context, key string) error {
	u, err := s.objectURL(key)
	if err != nil {
		return err
	}
	resp, err := s.do(ctx, http.MethodDelete, u, nil, EmptyPayloadHash, nil)
	if err != nil {
		return err
	}
	if !ok(resp) {
		return errorFrom("delete", key, resp)
	}
	resp.Body.Close()
	return nil
}

// SignedURL presigns a GET so a runner can fetch bytes without holding a
// credential and without proxying through the control plane.
func (s *Store) SignedURL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		return "", fmt.Errorf("blob/s3: signed URL ttl must be positive, got %s", ttl)
	}
	u, err := s.objectURL(key)
	if err != nil {
		return "", err
	}
	return presign(http.MethodGet, u, u.Host, s.creds, s.cfg.Region, "s3", ttl, s.now()), nil
}
