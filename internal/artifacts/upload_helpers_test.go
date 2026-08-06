package artifacts_test

// The upload sequence @actions/artifact performs, replayed by hand so the
// tests drive the same calls a real client does.

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/artifacts"
)

// uploadBlocks replays BlockBlobClient.uploadStream: stage each chunk with
// comp=block, then commit the ordered list with comp=blocklist.
func uploadBlocks(t *testing.T, client *http.Client, uploadURL string, body []byte, chunk int) {
	t.Helper()
	var ids []string
	for off := 0; off < len(body); off += chunk {
		end := min(off+chunk, len(body))
		id := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("block-%08d", off)))
		ids = append(ids, id)

		req, err := http.NewRequest(http.MethodPut, uploadURL+"&comp=block&blockid="+id, bytes.NewReader(body[off:end]))
		require.NoError(t, err)
		req.Header.Set("x-ms-version", "2021-08-06")
		resp, err := client.Do(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, resp.StatusCode, "staging a block must return 201")
		resp.Body.Close()
	}

	var list strings.Builder
	list.WriteString(`<?xml version="1.0" encoding="utf-8"?><BlockList>`)
	for _, id := range ids {
		fmt.Fprintf(&list, "<Latest>%s</Latest>", id)
	}
	list.WriteString("</BlockList>")

	req, err := http.NewRequest(http.MethodPut, uploadURL+"&comp=blocklist", strings.NewReader(list.String()))
	require.NoError(t, err)
	req.Header.Set("x-ms-blob-content-type", "application/zip")
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode, "committing the block list must return 201")
	assert.NotEmpty(t, resp.Header.Get("ETag"))
}

func zipBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		require.NoError(t, err)
		_, err = w.Write([]byte(body))
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

// uploadOne runs the whole upload sequence and returns the stored bytes.
func uploadOne(t *testing.T, h *harness, name string, files map[string]string) []byte {
	t.Helper()
	runBackend, jobBackend := backendIDs()
	resp := h.twirp(t, "CreateArtifact", map[string]any{
		"workflow_run_backend_id":     runBackend,
		"workflow_job_run_backend_id": jobBackend,
		"name":                        name,
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	created := decode[artifacts.CreateArtifactResponse](t, resp)

	payload := zipBytes(t, files)
	uploadBlocks(t, h.srv.Client(), created.SignedUploadURL, payload, 1<<20)

	sum := sha256.Sum256(payload)
	fin := h.twirp(t, "FinalizeArtifact", map[string]any{
		"workflow_run_backend_id":     runBackend,
		"workflow_job_run_backend_id": jobBackend,
		"name":                        name,
		"size":                        fmt.Sprintf("%d", len(payload)),
		"hash":                        "sha256:" + hex.EncodeToString(sum[:]),
	})
	require.Equal(t, http.StatusOK, fin.StatusCode)
	return payload
}
