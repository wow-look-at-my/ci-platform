// Package webui serves the embedded dashboard: correct content types, an
// immutable cache for content-hashed asset URLs, and an SPA fallback so a
// deep link like /#/jobs/12 survives a reload.
package webui

import (
	"errors"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/wow-look-at-my/ci-platform/web"
)

// Handler serves one filesystem of built assets.
type Handler struct {
	files fs.FS
	index []byte
}

// New serves the bundle committed under web/.
func New() (*Handler, error) { return NewFS(web.FS) }

// NewFS serves an arbitrary asset filesystem, for tests.
func NewFS(files fs.FS) (*Handler, error) {
	index, err := fs.ReadFile(files, "index.html")
	if err != nil {
		// Without index.html there is no UI to serve, and answering 200 with
		// nothing would look like a working deployment.
		return nil, errors.New("webui: index.html is missing from the embedded bundle; run: go run ./cmd/buildweb")
	}
	return &Handler{files: files, index: index}, nil
}

var contentTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".mjs":   "text/javascript; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".map":   "application/json; charset=utf-8",
	".svg":   "image/svg+xml",
	".json":  "application/json; charset=utf-8",
	".ico":   "image/x-icon",
	".woff2": "font/woff2",
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "the UI serves GET and HEAD only", http.StatusMethodNotAllowed)
		return
	}

	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" || name == "." {
		h.serveIndex(w, r)
		return
	}
	// An API path reaching here means the mux is misrouted. Serving HTML to an
	// API client would look like a malformed response instead of a 404.
	if strings.HasPrefix(name, "api/") || name == "healthz" {
		http.Error(w, `{"error":"not_found","message":"no such API endpoint"}`, http.StatusNotFound)
		return
	}

	body, err := fs.ReadFile(h.files, name)
	if err != nil {
		// A missing file with an extension is a genuine 404; anything else is
		// a client-side route, which index.html handles.
		if path.Ext(name) != "" {
			http.NotFound(w, r)
			return
		}
		h.serveIndex(w, r)
		return
	}
	h.write(w, r, name, body, cacheFor(name, r))
}

func (h *Handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	// index.html names the hashed asset URLs, so it must never be cached.
	h.write(w, r, "index.html", h.index, "no-store")
}

// cacheFor returns the Cache-Control for one asset. A request carrying the
// content hash (?v=...) can be cached forever, because a rebuilt bundle is a
// different URL. Everything else revalidates.
func cacheFor(name string, r *http.Request) string {
	if name == "index.html" {
		return "no-store"
	}
	if r.URL.Query().Get("v") != "" {
		return "public, max-age=31536000, immutable"
	}
	return "no-cache"
}

func (h *Handler) write(w http.ResponseWriter, r *http.Request, name string, body []byte, cache string) {
	ct := contentTypes[path.Ext(name)]
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", cache)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}

// Mount registers the UI on mux as the catch-all, leaving more specific
// patterns (the API, /healthz) to win on their own.
func (h *Handler) Mount(mux *http.ServeMux) {
	// Registered without a method so it cannot conflict with a method-specific
	// API pattern; non-read methods are rejected by the handler itself.
	mux.Handle("/", h)
}
