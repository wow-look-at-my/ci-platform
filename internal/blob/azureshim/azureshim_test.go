package azureshim_test

import (
	"context"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/blob"
	"github.com/wow-look-at-my/ci-platform/internal/blob/azureshim"
	"github.com/wow-look-at-my/ci-platform/internal/blob/disk"
)

type commit struct {
	ref         string
	size        int64
	digest      string
	contentType string
}

func newServer(t *testing.T, mutate func(*azureshim.Options)) (*httptest.Server, *disk.Store, *[]commit) {
	t.Helper()
	bs, err := disk.New(t.TempDir())
	require.NoError(t, err)
	var commits []commit

	opts := azureshim.Options{
		Store: bs,
		Resolve: func(r *http.Request) (azureshim.Target, *azureshim.Error) {
			if r.URL.Query().Get("token") != "good" {
				return azureshim.Target{}, azureshim.Errorf(http.StatusForbidden, "AuthenticationFailed", "bad token")
			}
			return azureshim.Target{Key: "objects/one", Ref: "1"}, nil
		},
		OnCommit: func(_ context.Context, tg azureshim.Target, size int64, digest, ct string) error {
			commits = append(commits, commit{tg.Ref, size, digest, ct})
			return nil
		},
	}
	if mutate != nil {
		mutate(&opts)
	}
	h, err := azureshim.New(opts)
	require.NoError(t, err)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, bs, &commits
}

func put(t *testing.T, srv *httptest.Server, query, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, srv.URL+"/container/blob/1?token=good"+query, strings.NewReader(body))
	require.NoError(t, err)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func blockID(n int) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("block-%03d", n)))
}

func TestStageAndCommitBlocks(t *testing.T) {
	srv, bs, commits := newServer(t, nil)

	for i, chunk := range []string{"hello ", "block ", "world"} {
		resp := put(t, srv, "&comp=block&blockid="+blockID(i), chunk, nil)
		require.Equal(t, http.StatusCreated, resp.StatusCode)
		assert.NotEmpty(t, resp.Header.Get("x-ms-version"))
		assert.NotEmpty(t, resp.Header.Get("x-ms-request-id"))
	}

	var list strings.Builder
	list.WriteString(`<?xml version="1.0" encoding="utf-8"?><BlockList>`)
	for i := range 3 {
		fmt.Fprintf(&list, "<Latest>%s</Latest>", blockID(i))
	}
	list.WriteString("</BlockList>")

	resp := put(t, srv, "&comp=blocklist", list.String(), map[string]string{
		"x-ms-blob-content-type": "application/zip",
	})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	assert.NotEmpty(t, resp.Header.Get("ETag"))
	assert.NotEmpty(t, resp.Header.Get("Last-Modified"))

	rc, err := bs.Get(context.Background(), "objects/one")
	require.NoError(t, err)
	defer rc.Close()
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, "hello block world", string(got))

	require.Len(t, *commits, 1)
	assert.Equal(t, int64(17), (*commits)[0].size)
	assert.Equal(t, "application/zip", (*commits)[0].contentType)
	assert.Len(t, (*commits)[0].digest, 64)
}

func TestBlockListOrderIsDocumentOrder(t *testing.T) {
	srv, bs, _ := newServer(t, nil)
	put(t, srv, "&comp=block&blockid="+blockID(0), "first", nil)
	put(t, srv, "&comp=block&blockid="+blockID(1), "second", nil)

	// Commit them in reverse; the committed blob follows the list, not the
	// order the blocks were staged in.
	body := fmt.Sprintf(`<BlockList><Latest>%s</Latest><Latest>%s</Latest></BlockList>`, blockID(1), blockID(0))
	require.Equal(t, http.StatusCreated, put(t, srv, "&comp=blocklist", body, nil).StatusCode)

	rc, err := bs.Get(context.Background(), "objects/one")
	require.NoError(t, err)
	defer rc.Close()
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, "secondfirst", string(got))
}

func TestSingleShotUpload(t *testing.T) {
	srv, bs, commits := newServer(t, nil)
	resp := put(t, srv, "", "whole object", map[string]string{"x-ms-blob-type": "BlockBlob"})
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	rc, err := bs.Get(context.Background(), "objects/one")
	require.NoError(t, err)
	defer rc.Close()
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, "whole object", string(got))
	assert.Len(t, *commits, 1)
}

func TestSingleShotRequiresBlockBlobType(t *testing.T) {
	srv, _, _ := newServer(t, nil)
	resp := put(t, srv, "", "x", map[string]string{"x-ms-blob-type": "PageBlob"})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Equal(t, "UnsupportedHeader", resp.Header.Get("x-ms-error-code"))

	bare := put(t, srv, "", "x", nil)
	assert.Equal(t, http.StatusBadRequest, bare.StatusCode)
}

func TestErrorsAreAzureShaped(t *testing.T) {
	srv, _, _ := newServer(t, nil)

	req, err := http.NewRequest(http.MethodPut, srv.URL+"/container/blob/1?token=bad", strings.NewReader("x"))
	require.NoError(t, err)
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Equal(t, "AuthenticationFailed", resp.Header.Get("x-ms-error-code"))
	assert.Equal(t, "application/xml", resp.Header.Get("Content-Type"))

	var body struct {
		XMLName xml.Name `xml:"Error"`
		Code    string   `xml:"Code"`
		Message string   `xml:"Message"`
	}
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, xml.Unmarshal(raw, &body))
	assert.Equal(t, "AuthenticationFailed", body.Code)
	assert.Equal(t, "bad token", body.Message)
}

func TestRejectedRequests(t *testing.T) {
	srv, _, _ := newServer(t, nil)

	t.Run("wrong method", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/container/blob/1?token=good", nil)
		require.NoError(t, err)
		resp, err := srv.Client().Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
	})

	t.Run("unknown comp", func(t *testing.T) {
		resp := put(t, srv, "&comp=snapshot", "", nil)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		assert.Equal(t, "InvalidQueryParameterValue", resp.Header.Get("x-ms-error-code"))
	})

	t.Run("block with no id", func(t *testing.T) {
		resp := put(t, srv, "&comp=block", "x", nil)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		assert.Equal(t, "MissingRequiredQueryParameter", resp.Header.Get("x-ms-error-code"))
	})

	t.Run("empty block list", func(t *testing.T) {
		resp := put(t, srv, "&comp=blocklist", `<BlockList></BlockList>`, nil)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		assert.Equal(t, "InvalidBlockList", resp.Header.Get("x-ms-error-code"))
	})

	t.Run("malformed block list", func(t *testing.T) {
		resp := put(t, srv, "&comp=blocklist", `<BlockList><Latest>`, nil)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("block list naming an unstaged block", func(t *testing.T) {
		resp := put(t, srv, "&comp=blocklist", `<BlockList><Latest>bm9wZQ==</Latest></BlockList>`, nil)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
		body, _ := io.ReadAll(resp.Body)
		assert.Contains(t, string(body), "never staged",
			"assembling around a missing block would produce a corrupt object that looks fine")
	})
}

func TestBlockCountLimit(t *testing.T) {
	srv, _, _ := newServer(t, func(o *azureshim.Options) { o.MaxBlocks = 2 })
	for i := range 3 {
		require.Equal(t, http.StatusCreated, put(t, srv, "&comp=block&blockid="+blockID(i), "x", nil).StatusCode)
	}
	body := fmt.Sprintf(`<BlockList><Latest>%s</Latest><Latest>%s</Latest><Latest>%s</Latest></BlockList>`,
		blockID(0), blockID(1), blockID(2))
	resp := put(t, srv, "&comp=blocklist", body, nil)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Equal(t, "BlockCountExceedsLimit", resp.Header.Get("x-ms-error-code"))
}

func TestOnCommitFailureFailsTheUpload(t *testing.T) {
	srv, _, _ := newServer(t, func(o *azureshim.Options) {
		o.OnCommit = func(context.Context, azureshim.Target, int64, string, string) error {
			return fmt.Errorf("metadata store unavailable")
		}
	})
	resp := put(t, srv, "", "x", map[string]string{"x-ms-blob-type": "BlockBlob"})
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode,
		"the client must not believe an upload landed when the record of it did not")
	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "metadata store unavailable")
}

func TestBadTargetKeyIsRejected(t *testing.T) {
	srv, _, _ := newServer(t, func(o *azureshim.Options) {
		o.Resolve = func(*http.Request) (azureshim.Target, *azureshim.Error) {
			return azureshim.Target{Key: "../escape"}, nil
		}
	})
	resp := put(t, srv, "", "x", map[string]string{"x-ms-blob-type": "BlockBlob"})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Equal(t, "InvalidUri", resp.Header.Get("x-ms-error-code"))
}

func TestStorageFailureIsReported(t *testing.T) {
	srv, _, _ := newServer(t, func(o *azureshim.Options) { o.Store = brokenStore{o.Store} })
	resp := put(t, srv, "&comp=block&blockid="+blockID(0), "x", nil)
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "bucket unreachable")
}

type brokenStore struct{ blob.Store }

func (brokenStore) Put(context.Context, string, io.Reader) (int64, string, error) {
	return 0, "", fmt.Errorf("bucket unreachable")
}

func TestNewValidatesOptions(t *testing.T) {
	bs, err := disk.New(t.TempDir())
	require.NoError(t, err)
	resolve := func(*http.Request) (azureshim.Target, *azureshim.Error) { return azureshim.Target{}, nil }
	onCommit := func(context.Context, azureshim.Target, int64, string, string) error { return nil }

	_, err = azureshim.New(azureshim.Options{Resolve: resolve, OnCommit: onCommit})
	assert.Error(t, err)
	_, err = azureshim.New(azureshim.Options{Store: bs, OnCommit: onCommit})
	assert.Error(t, err)
	_, err = azureshim.New(azureshim.Options{Store: bs, Resolve: resolve})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "silently vanishes")
}

func TestErrorString(t *testing.T) {
	e := azureshim.Errorf(http.StatusBadRequest, "BadThing", "it was %d", 42)
	assert.Equal(t, "BadThing: it was 42", e.Error())
}
