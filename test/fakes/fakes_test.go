package fakes

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegistry_ServesUntilToldOtherwise(t *testing.T) {
	r := NewRegistry()
	defer r.Close()

	resp, err := http.Get(r.URL() + "/v2/")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	resp2, err := http.Get(r.URL() + "/v2/img/blobs/sha256:abc")
	require.NoError(t, err)
	defer resp2.Body.Close()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)
	assert.Equal(t, int64(1), r.BlobRequests())
}

// The body a real Cloudflare 524 returns says nothing about the registry or the
// build, which is exactly why it needs classifying rather than displaying.
func TestRegistry_Cloudflare524(t *testing.T) {
	r := NewRegistry()
	defer r.Close()
	r.SetFault(FaultCloudflare524, 0)

	resp, err := http.Get(r.URL() + "/v2/img/blobs/uploads/1")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, 524, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), "error code: 524")
	assert.Equal(t, "cloudflare", resp.Header.Get("Server"))
}

// failFirstN is what lets a test prove a retry recovered rather than merely
// that a retry was attempted.
func TestRegistry_RecoversAfterN(t *testing.T) {
	r := NewRegistry()
	defer r.Close()
	r.SetFault(FaultServiceUnavailable, 2)

	codes := []int{}
	for range 3 {
		resp, err := http.Get(r.URL() + "/v2/img/blobs/x")
		require.NoError(t, err)
		codes = append(codes, resp.StatusCode)
		resp.Body.Close()
	}
	assert.Equal(t, []int{503, 503, 200}, codes)
	assert.Equal(t, int64(3), r.BlobRequests())
}

func TestRegistry_RateLimitAndHangUp(t *testing.T) {
	r := NewRegistry()
	defer r.Close()

	r.SetFault(FaultRateLimit, 0)
	resp, err := http.Get(r.URL() + "/v2/img/blobs/x")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	assert.Equal(t, "1", resp.Header.Get("Retry-After"))

	r.SetFault(FaultHangUp, 0)
	resp2, err := http.Get(r.URL() + "/v2/img/blobs/x")
	if err == nil {
		_, readErr := io.ReadAll(resp2.Body)
		resp2.Body.Close()
		// A truncated body is the point: the client sees an unexpected EOF.
		assert.Error(t, readErr)
	}

	assert.NotEmpty(t, r.Host())
	assert.Positive(t, r.Requests())
}

func TestGitHub_RecordsCheckRunsAndCoalescing(t *testing.T) {
	g := NewGitHub()
	defer g.Close()

	id := postJSON(t, g.URL()+"/repos/o/r/check-runs", map[string]any{
		"name": "build (linux)", "head_sha": "abc", "status": "queued",
	})
	require.NotZero(t, id)

	patchJSON(t, g.URL()+"/repos/o/r/check-runs/"+itoa(id), map[string]any{
		"status":     "completed",
		"conclusion": "action_required",
		"output": map[string]any{
			"title":   "Infrastructure failure",
			"summary": "classified infra",
			"text":    "| step | conclusion |",
			"annotations": []any{
				map[string]any{"path": "main.go", "start_line": 3, "annotation_level": "failure", "message": "boom"},
			},
		},
		"actions": []any{
			map[string]any{"label": "Re-run job", "description": "Run it again", "identifier": "rerun"},
		},
	})

	cr, ok := g.CheckRunNamed("build (linux)")
	require.True(t, ok)
	assert.Equal(t, "completed", cr.Status)
	assert.Equal(t, "action_required", cr.Conclusion)
	assert.Equal(t, "abc", cr.HeadSHA)
	assert.Equal(t, 2, cr.Updates)
	require.Len(t, cr.Output.Annotations, 1)
	require.Len(t, cr.Actions, 1)
	assert.Equal(t, "rerun", cr.Actions[0].Identifier)

	_, missing := g.CheckRunNamed("nope")
	assert.False(t, missing)
}

func TestGitHub_RecordsStatuses(t *testing.T) {
	g := NewGitHub()
	defer g.Close()

	postJSON(t, g.URL()+"/repos/o/r/statuses/deadbeef", map[string]any{
		"state": "error", "context": "build (linux)", "description": "infra",
	})
	sts := g.Statuses()
	require.Len(t, sts, 1)
	assert.Equal(t, "deadbeef", sts[0].SHA)
	assert.Equal(t, "error", sts[0].State)
	assert.Equal(t, "build (linux)", sts[0].Context)
}

func TestGitHub_FailNextDrivesRetryBehaviour(t *testing.T) {
	g := NewGitHub()
	defer g.Close()
	g.FailNext(2, http.StatusBadGateway)

	for range 2 {
		resp, err := http.Get(g.URL() + "/anything")
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
	}
	resp, err := http.Get(g.URL() + "/anything")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestGitHub_ServesRepoFilesAndDirectoryListings(t *testing.T) {
	g := NewGitHub()
	defer g.Close()
	g.AddFile(".github/workflows/ci.yml", "name: CI\non: push\n")

	resp, err := http.Get(g.URL() + "/repos/o/r/contents/.github/workflows/ci.yml")
	require.NoError(t, err)
	defer resp.Body.Close()
	var file map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&file))
	assert.Equal(t, "file", file["type"])
	assert.Equal(t, encodeBase64("name: CI\non: push\n"), file["content"])

	listResp, err := http.Get(g.URL() + "/repos/o/r/contents/.github/workflows")
	require.NoError(t, err)
	defer listResp.Body.Close()
	var list []map[string]any
	require.NoError(t, json.NewDecoder(listResp.Body).Decode(&list))
	require.Len(t, list, 1)
	assert.Equal(t, "ci.yml", list[0]["name"])

	miss, err := http.Get(g.URL() + "/repos/o/r/contents/nope/at/all")
	require.NoError(t, err)
	miss.Body.Close()
	assert.Equal(t, http.StatusNotFound, miss.StatusCode)
}

func TestGitHub_InstallationTokenAndSignature(t *testing.T) {
	g := NewGitHub()
	defer g.Close()

	id := postJSON(t, g.URL()+"/app/installations/1/access_tokens", map[string]any{})
	assert.Zero(t, id, "token response carries no check run id")

	body := []byte(`{"action":"opened"}`)
	sig := g.SignWebhook(body)
	assert.True(t, g.VerifyWebhookSignature(body, sig))
	assert.False(t, g.VerifyWebhookSignature(body, "sha256=deadbeef"))
	assert.Positive(t, g.Requests())
}

func postJSON(t *testing.T, url string, body map[string]any) int64 {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	require.NoError(t, err)
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if id, ok := out["id"].(float64); ok {
		return int64(id)
	}
	return 0
}

func patchJSON(t *testing.T, url string, body map[string]any) {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPatch, url, bytes.NewReader(b))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func itoa(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}
