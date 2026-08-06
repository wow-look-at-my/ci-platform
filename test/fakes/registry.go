// Package fakes provides test doubles for the chaos and end-to-end suites: a
// container registry that can be told to fail the way real ones fail, and a
// GitHub API server that records what the platform reported.
package fakes

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
)

// RegistryFault selects how the fake registry misbehaves.
type RegistryFault string

const (
	// FaultNone serves normally.
	FaultNone RegistryFault = ""
	// FaultCloudflare524 reproduces the incident that started this project: a
	// blob upload killed by a Cloudflare origin timeout, whose body says
	// nothing about the build.
	FaultCloudflare524 RegistryFault = "cloudflare-524"
	// FaultServiceUnavailable returns a bare 503.
	FaultServiceUnavailable RegistryFault = "503"
	// FaultRateLimit returns 429 with a Retry-After.
	FaultRateLimit RegistryFault = "429"
	// FaultHangUp closes the connection mid-response.
	FaultHangUp RegistryFault = "hangup"
)

// Registry is a fake container registry whose failure mode can be changed at
// runtime, and which can be told to recover after N attempts so a test can
// assert that a retry actually succeeded rather than merely being attempted.
type Registry struct {
	srv *httptest.Server

	mu           sync.Mutex
	fault        RegistryFault
	failFirstN   int
	blobRequests atomic.Int64
	allRequests  atomic.Int64
}

// NewRegistry starts a fake registry. Close it with Close.
func NewRegistry() *Registry {
	r := &Registry{}
	r.srv = httptest.NewServer(http.HandlerFunc(r.serve))
	return r
}

// URL is the registry's base URL.
func (r *Registry) URL() string { return r.srv.URL }

// Host is the registry's host:port, which is what an image reference needs.
func (r *Registry) Host() string { return strings.TrimPrefix(r.srv.URL, "http://") }

// Close shuts the server down.
func (r *Registry) Close() { r.srv.Close() }

// SetFault makes every subsequent blob request fail in the given way. A
// failFirstN of 0 means fail forever; a positive value means fail that many
// times and then serve normally, which is how a test proves a retry recovered.
func (r *Registry) SetFault(f RegistryFault, failFirstN int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fault = f
	r.failFirstN = failFirstN
}

// BlobRequests counts blob upload/download attempts, so a test can assert the
// exact number of retries rather than just that it eventually worked.
func (r *Registry) BlobRequests() int64 { return r.blobRequests.Load() }

// Requests counts every request the registry received.
func (r *Registry) Requests() int64 { return r.allRequests.Load() }

// nextFault reports the fault to apply to this request and consumes one of the
// failFirstN budget.
func (r *Registry) nextFault(n int64) RegistryFault {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fault == FaultNone {
		return FaultNone
	}
	if r.failFirstN > 0 && n > int64(r.failFirstN) {
		return FaultNone
	}
	return r.fault
}

func (r *Registry) serve(w http.ResponseWriter, req *http.Request) {
	r.allRequests.Add(1)

	// /v2/ is the registry API version probe; it must always succeed or the
	// client gives up before reaching the interesting failure.
	if req.URL.Path == "/v2/" {
		w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
		w.WriteHeader(http.StatusOK)
		return
	}

	isBlob := strings.Contains(req.URL.Path, "/blobs/") || strings.Contains(req.URL.Path, "/uploads/")
	if !isBlob {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{}`)
		return
	}

	n := r.blobRequests.Add(1)
	switch r.nextFault(n) {
	case FaultCloudflare524:
		// Cloudflare's own 524 body: an HTML error page from the edge, with
		// nothing in it about the registry or the build.
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Server", "cloudflare")
		w.WriteHeader(524)
		fmt.Fprint(w, "<html><head><title>524: A timeout occurred</title></head>"+
			"<body>error code: 524</body></html>")
	case FaultServiceUnavailable:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"errors":[{"code":"UNAVAILABLE","message":"service unavailable"}]}`)
	case FaultRateLimit:
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"errors":[{"code":"TOOMANYREQUESTS","message":"rate limit exceeded"}]}`)
	case FaultHangUp:
		// Take the connection away mid-response so the client sees an
		// unexpected EOF rather than a status code.
		hj, ok := w.(http.Hijacker)
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 1024\r\n\r\npartial"))
		_ = conn.Close()
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	}
}
