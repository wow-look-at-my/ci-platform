package webui

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html": {Data: []byte(`<!doctype html><script src="/app.mjs?v=abc123"></script>`)},
		"app.mjs":    {Data: []byte("export const x = 1;\n")},
		"app.css":    {Data: []byte(":root{--x:1}\n")},
	}
}

func get(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", target, nil))
	return w
}

func TestServesTheEmbeddedBundle(t *testing.T) {
	h, err := New()
	require.NoError(t, err)

	w := get(t, h, "/")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "text/html")
	assert.Contains(t, w.Body.String(), "app.mjs?v=")

	w = get(t, h, "/app.mjs")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "text/javascript; charset=utf-8", w.Header().Get("Content-Type"))

	w = get(t, h, "/app.css")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "text/css; charset=utf-8", w.Header().Get("Content-Type"))
}

func TestContentTypesAndHeaders(t *testing.T) {
	h, err := NewFS(testFS())
	require.NoError(t, err)

	w := get(t, h, "/app.mjs")
	assert.Equal(t, "text/javascript; charset=utf-8", w.Header().Get("Content-Type"))
	assert.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"))
	assert.Equal(t, "20", w.Header().Get("Content-Length"))
}

func TestCacheHeaders(t *testing.T) {
	h, err := NewFS(testFS())
	require.NoError(t, err)

	assert.Equal(t, "no-store", get(t, h, "/").Header().Get("Cache-Control"),
		"index.html names the hashed URLs, so caching it would pin the old bundle")
	assert.Equal(t, "no-cache", get(t, h, "/app.mjs").Header().Get("Cache-Control"))
	assert.Equal(t, "public, max-age=31536000, immutable", get(t, h, "/app.mjs?v=abc123").Header().Get("Cache-Control"))
}

func TestSPAFallback(t *testing.T) {
	h, err := NewFS(testFS())
	require.NoError(t, err)

	for _, target := range []string{"/runs", "/jobs/12", "/deeply/nested/route"} {
		w := get(t, h, target)
		assert.Equal(t, http.StatusOK, w.Code, target)
		assert.Contains(t, w.Header().Get("Content-Type"), "text/html", target)
	}
}

func TestMissingAssetIs404NotHTML(t *testing.T) {
	h, err := NewFS(testFS())
	require.NoError(t, err)
	w := get(t, h, "/missing.css")
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.NotContains(t, w.Body.String(), "<!doctype")
}

func TestAPIPathsNeverGetTheSPAFallback(t *testing.T) {
	h, err := NewFS(testFS())
	require.NoError(t, err)
	for _, target := range []string{"/api/v1/runs", "/healthz"} {
		w := get(t, h, target)
		assert.Equal(t, http.StatusNotFound, w.Code, target)
		assert.NotContains(t, w.Body.String(), "<!doctype", target)
	}
}

func TestPathTraversalCannotEscapeTheBundle(t *testing.T) {
	h, err := NewFS(testFS())
	require.NoError(t, err)
	w := get(t, h, "/../../etc/passwd")
	assert.NotContains(t, w.Body.String(), "root:")
}

func TestHeadHasHeadersButNoBody(t *testing.T) {
	h, err := NewFS(testFS())
	require.NoError(t, err)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("HEAD", "/app.css", nil))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "text/css; charset=utf-8", w.Header().Get("Content-Type"))
	assert.Empty(t, w.Body.String())
}

func TestNonReadMethodsAreRejected(t *testing.T) {
	h, err := NewFS(testFS())
	require.NoError(t, err)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/", nil))
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
	assert.Equal(t, "GET, HEAD", w.Header().Get("Allow"))
}

func TestAMissingIndexIsAStartupFailureNotAnEmptyPage(t *testing.T) {
	_, err := NewFS(fstest.MapFS{"app.mjs": {Data: []byte("x")}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "buildweb")
}

func TestMountLeavesMoreSpecificPatternsAlone(t *testing.T) {
	h, err := NewFS(testFS())
	require.NoError(t, err)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/runs", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	h.Mount(mux)

	assert.Equal(t, http.StatusTeapot, get(t, mux, "/api/v1/runs").Code)
	assert.Equal(t, http.StatusOK, get(t, mux, "/runs").Code)
}
