package artifacts_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/artifacts"
	"github.com/wow-look-at-my/ci-platform/internal/blob/disk"
	"github.com/wow-look-at-my/ci-platform/internal/jobtoken"
)

const (
	runID   = int64(42)
	jobID   = int64(7)
	attempt = 1
)

type harness struct {
	svc    *artifacts.Service
	store  *fakeStore
	blob   *disk.Store
	signer *jobtoken.Signer
	srv    *httptest.Server
	token  string
}

func newHarness(t *testing.T, mutate func(*artifacts.Options)) *harness {
	t.Helper()
	fs := newFakeStore()
	bs, err := disk.New(t.TempDir())
	require.NoError(t, err)
	signer, err := jobtoken.New(jobtoken.Options{
		Key:    []byte("0123456789abcdef0123456789abcdef"),
		Issuer: "https://ci.example.ghe.com",
		Lookup: func(int64, int64, int) (jobtoken.Job, error) {
			return jobtoken.Job{
				RepoID: 9, Repo: "wow-look-at-my/ci-platform",
				Scopes: jobtoken.DefaultScopes, ExpiresAt: time.Now().Add(time.Hour),
			}, nil
		},
	})
	require.NoError(t, err)

	h := &harness{store: fs, blob: bs, signer: signer}
	// The service needs its own public URL before the test server exists, so
	// the server is created first with a placeholder handler.
	mux := http.NewServeMux()
	h.srv = httptest.NewServer(mux)
	t.Cleanup(h.srv.Close)

	opts := artifacts.Options{
		Store: fs, Blob: bs, Signer: signer, BaseURL: h.srv.URL,
		RepoQuotaBytes: 1 << 30,
		RepoUsage:      func(context.Context, int64) (int64, error) { return 0, nil },
	}
	if mutate != nil {
		mutate(&opts)
	}
	svc, err := artifacts.New(opts)
	require.NoError(t, err)
	h.svc = svc
	mux.Handle("/", svc.Handler())

	h.token, err = signer.Mint(runID, jobID, attempt)
	require.NoError(t, err)
	return h
}

// twirp posts a Results API request the way ArtifactServiceClientJSON does.
func (h *harness) twirp(t *testing.T, method string, body any) *http.Response {
	t.Helper()
	return h.twirpAs(t, h.token, method, body)
}

// twirpAs is twirp with a caller-chosen token, for the scope checks.
func (h *harness) twirpAs(t *testing.T, token, method string, body any) *http.Response {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+artifacts.TwirpPrefix+method, bytes.NewReader(raw))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := h.srv.Client().Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func decode[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	var v T
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&v))
	return v
}

func backendIDs() (string, string) {
	return jobtoken.BackendRunID(runID), jobtoken.BackendJobID(jobID, attempt)
}

// TestUploadArtifactV4EndToEnd drives the exact call sequence
// @actions/artifact's uploadArtifact makes: CreateArtifact, an Azure block
// upload of the zip, then FinalizeArtifact with the size and sha256.
func TestUploadArtifactV4EndToEnd(t *testing.T) {
	h := newHarness(t, nil)
	runBackend, jobBackend := backendIDs()

	resp := h.twirp(t, "CreateArtifact", map[string]any{
		"workflow_run_backend_id":     runBackend,
		"workflow_job_run_backend_id": jobBackend,
		"name":                        "build-output",
		"version":                     7,
		"mime_type":                   "application/zip",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	created := decode[artifacts.CreateArtifactResponse](t, resp)
	require.True(t, created.OK)
	require.NotEmpty(t, created.SignedUploadURL)

	payload := zipBytes(t, map[string]string{"out/app.txt": "the binary"})
	uploadBlocks(t, h.srv.Client(), created.SignedUploadURL, payload, 16)

	sum := sha256.Sum256(payload)
	fin := h.twirp(t, "FinalizeArtifact", map[string]any{
		"workflow_run_backend_id":     runBackend,
		"workflow_job_run_backend_id": jobBackend,
		"name":                        "build-output",
		"size":                        fmt.Sprintf("%d", len(payload)),
		"hash":                        "sha256:" + hex.EncodeToString(sum[:]),
	})
	require.Equal(t, http.StatusOK, fin.StatusCode)
	finalized := decode[artifacts.FinalizeArtifactResponse](t, fin)
	assert.True(t, finalized.OK)
	assert.Equal(t, "1", finalized.ArtifactID, "artifact_id is an int64, so a string on the wire")

	stored, err := h.store.GetArtifact(context.Background(), 1)
	require.NoError(t, err)
	assert.True(t, stored.Finalized)
	assert.Equal(t, int64(len(payload)), stored.SizeBytes)
	assert.Equal(t, "sha256:"+hex.EncodeToString(sum[:]), stored.Digest)
	assert.NotEmpty(t, h.store.eventsOfKind("artifact_finalized"))
}

// TestDownloadArtifactV4EndToEnd drives downloadArtifactInternal: ListArtifacts
// with an id filter, GetSignedArtifactURL, then an unauthenticated GET.
func TestDownloadArtifactV4EndToEnd(t *testing.T) {
	h := newHarness(t, nil)
	runBackend, jobBackend := backendIDs()
	payload := uploadOne(t, h, "build-output", map[string]string{"out/app.txt": "the binary"})

	list := h.twirp(t, "ListArtifacts", map[string]any{
		"workflow_run_backend_id":     runBackend,
		"workflow_job_run_backend_id": jobBackend,
		"id_filter":                   "1",
	})
	require.Equal(t, http.StatusOK, list.StatusCode)
	listed := decode[artifacts.ListArtifactsResponse](t, list)
	require.Len(t, listed.Artifacts, 1)
	assert.Equal(t, "build-output", listed.Artifacts[0].Name)
	assert.Equal(t, "1", listed.Artifacts[0].DatabaseID)
	assert.Equal(t, fmt.Sprintf("%d", len(payload)), listed.Artifacts[0].Size)
	assert.Equal(t, runBackend, listed.Artifacts[0].WorkflowRunBackendID)

	signed := h.twirp(t, "GetSignedArtifactURL", map[string]any{
		"workflow_run_backend_id":     runBackend,
		"workflow_job_run_backend_id": jobBackend,
		"name":                        "build-output",
	})
	require.Equal(t, http.StatusOK, signed.StatusCode)
	url := decode[artifacts.GetSignedArtifactURLResponse](t, signed).SignedURL
	require.NotEmpty(t, url)

	// The client downloads with a fresh HTTP client that sends no
	// Authorization header, so the URL must authenticate itself.
	resp, err := http.Get(url)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/zip", resp.Header.Get("Content-Type"))
	assert.Contains(t, resp.Header.Get("Content-Disposition"), "build-output.zip")

	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, payload, got)
}

func TestSingleShotBlobUpload(t *testing.T) {
	h := newHarness(t, nil)
	runBackend, jobBackend := backendIDs()

	resp := h.twirp(t, "CreateArtifact", map[string]any{
		"workflow_run_backend_id":     runBackend,
		"workflow_job_run_backend_id": jobBackend,
		"name":                        "raw",
	})
	created := decode[artifacts.CreateArtifactResponse](t, resp)

	req, err := http.NewRequest(http.MethodPut, created.SignedUploadURL, strings.NewReader("raw bytes"))
	require.NoError(t, err)
	req.Header.Set("x-ms-blob-type", "BlockBlob")
	up, err := h.srv.Client().Do(req)
	require.NoError(t, err)
	defer up.Body.Close()
	require.Equal(t, http.StatusCreated, up.StatusCode)

	fin := h.twirp(t, "FinalizeArtifact", map[string]any{
		"workflow_run_backend_id":     runBackend,
		"workflow_job_run_backend_id": jobBackend,
		"name":                        "raw",
		"size":                        "9",
	})
	assert.Equal(t, http.StatusOK, fin.StatusCode)
}

func TestUploadRejectsUnsignedAndTamperedURLs(t *testing.T) {
	h := newHarness(t, nil)
	runBackend, jobBackend := backendIDs()
	resp := h.twirp(t, "CreateArtifact", map[string]any{
		"workflow_run_backend_id":     runBackend,
		"workflow_job_run_backend_id": jobBackend,
		"name":                        "guarded",
	})
	uploadURL := decode[artifacts.CreateArtifactResponse](t, resp).SignedUploadURL

	put := func(t *testing.T, target string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPut, target, strings.NewReader("x"))
		require.NoError(t, err)
		req.Header.Set("x-ms-blob-type", "BlockBlob")
		r, err := h.srv.Client().Do(req)
		require.NoError(t, err)
		t.Cleanup(func() { r.Body.Close() })
		return r
	}

	t.Run("no signature", func(t *testing.T) {
		r := put(t, h.srv.URL+artifacts.PathUpload+"1")
		assert.Equal(t, http.StatusForbidden, r.StatusCode)
		assert.Equal(t, "AuthenticationFailed", r.Header.Get("x-ms-error-code"))
		body, _ := io.ReadAll(r.Body)
		assert.Contains(t, string(body), "<Error>", "the SDK parses Azure's XML error shape")
	})
	t.Run("signature for another artifact", func(t *testing.T) {
		tampered := strings.Replace(uploadURL, artifacts.PathUpload+"1", artifacts.PathUpload+"2", 1)
		assert.Equal(t, http.StatusForbidden, put(t, tampered).StatusCode)
	})
}

func TestBlockListNamingAnUnstagedBlockIsRefused(t *testing.T) {
	h := newHarness(t, nil)
	runBackend, jobBackend := backendIDs()
	resp := h.twirp(t, "CreateArtifact", map[string]any{
		"workflow_run_backend_id":     runBackend,
		"workflow_job_run_backend_id": jobBackend,
		"name":                        "gappy",
	})
	uploadURL := decode[artifacts.CreateArtifactResponse](t, resp).SignedUploadURL

	body := `<?xml version="1.0"?><BlockList><Latest>bmV2ZXItc3RhZ2Vk</Latest></BlockList>`
	req, err := http.NewRequest(http.MethodPut, uploadURL+"&comp=blocklist", strings.NewReader(body))
	require.NoError(t, err)
	r, err := h.srv.Client().Do(req)
	require.NoError(t, err)
	defer r.Body.Close()

	assert.Equal(t, http.StatusBadRequest, r.StatusCode)
	assert.Equal(t, "InvalidBlockList", r.Header.Get("x-ms-error-code"))

	// Nothing was committed, so finalize has nothing to find.
	fin := h.twirp(t, "FinalizeArtifact", map[string]any{
		"workflow_run_backend_id":     runBackend,
		"workflow_job_run_backend_id": jobBackend,
		"name":                        "gappy",
		"size":                        "0",
	})
	assert.Equal(t, http.StatusBadRequest, fin.StatusCode)
	assert.Contains(t, decode[map[string]string](t, fin)["msg"], "no content was uploaded")
}

func TestFinalizeRejectsDigestMismatch(t *testing.T) {
	h := newHarness(t, nil)
	runBackend, jobBackend := backendIDs()
	resp := h.twirp(t, "CreateArtifact", map[string]any{
		"workflow_run_backend_id": runBackend, "workflow_job_run_backend_id": jobBackend, "name": "corrupt",
	})
	created := decode[artifacts.CreateArtifactResponse](t, resp)
	uploadBlocks(t, h.srv.Client(), created.SignedUploadURL, []byte("actual bytes"), 4)

	fin := h.twirp(t, "FinalizeArtifact", map[string]any{
		"workflow_run_backend_id": runBackend, "workflow_job_run_backend_id": jobBackend,
		"name": "corrupt", "size": "12",
		"hash": "sha256:" + strings.Repeat("0", 64),
	})
	require.Equal(t, http.StatusBadRequest, fin.StatusCode)
	body := decode[map[string]string](t, fin)
	assert.Equal(t, artifacts.CodeInvalidArgument, body["code"])
	assert.Contains(t, body["msg"], "the upload was corrupted")

	stored, err := h.store.GetArtifact(context.Background(), 1)
	require.NoError(t, err)
	assert.False(t, stored.Finalized, "a corrupt upload must not be recorded as good")
}

func TestFinalizeRejectsSizeMismatch(t *testing.T) {
	h := newHarness(t, nil)
	runBackend, jobBackend := backendIDs()
	resp := h.twirp(t, "CreateArtifact", map[string]any{
		"workflow_run_backend_id": runBackend, "workflow_job_run_backend_id": jobBackend, "name": "short",
	})
	created := decode[artifacts.CreateArtifactResponse](t, resp)
	uploadBlocks(t, h.srv.Client(), created.SignedUploadURL, []byte("twelve bytes"), 4)

	fin := h.twirp(t, "FinalizeArtifact", map[string]any{
		"workflow_run_backend_id": runBackend, "workflow_job_run_backend_id": jobBackend,
		"name": "short", "size": "999",
	})
	require.Equal(t, http.StatusBadRequest, fin.StatusCode)
	assert.Contains(t, decode[map[string]string](t, fin)["msg"], "declared as 999 bytes but 12 were stored")
}

// TestCrossRunAccessIsDenied is the isolation case: a token minted for one run
// cannot name another run's backend ids.
func TestCrossRunAccessIsDenied(t *testing.T) {
	h := newHarness(t, nil)
	_, jobBackend := backendIDs()

	for _, method := range []string{"CreateArtifact", "FinalizeArtifact", "ListArtifacts", "GetSignedArtifactURL", "DeleteArtifact"} {
		t.Run(method, func(t *testing.T) {
			resp := h.twirp(t, method, map[string]any{
				"workflow_run_backend_id":     jobtoken.BackendRunID(999),
				"workflow_job_run_backend_id": jobBackend,
				"name":                        "whatever",
				"size":                        "1",
			})
			require.Equal(t, http.StatusForbidden, resp.StatusCode)
			body := decode[map[string]string](t, resp)
			assert.Equal(t, artifacts.CodePermissionDenied, body["code"])
			assert.Contains(t, body["msg"], "does not belong to this job token")
		})
	}
}

func TestOtherAttemptCannotWriteThisAttemptsArtifacts(t *testing.T) {
	h := newHarness(t, nil)
	runBackend, _ := backendIDs()
	resp := h.twirp(t, "CreateArtifact", map[string]any{
		"workflow_run_backend_id":     runBackend,
		"workflow_job_run_backend_id": jobtoken.BackendJobID(jobID, 2),
		"name":                        "x",
	})
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestUnauthenticatedTwirpIsTwirpShaped(t *testing.T) {
	h := newHarness(t, nil)
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+artifacts.TwirpPrefix+"CreateArtifact", strings.NewReader("{}"))
	require.NoError(t, err)
	resp, err := h.srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	body := decode[map[string]string](t, resp)
	assert.Equal(t, artifacts.CodeUnauthenticated, body["code"])
	assert.NotEmpty(t, body["msg"], "the client surfaces body.msg; an empty one tells the operator nothing")
}

func TestUnknownTwirpMethod(t *testing.T) {
	h := newHarness(t, nil)
	resp := h.twirp(t, "ExplodeArtifact", map[string]any{})
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Equal(t, artifacts.CodeNotFound, decode[map[string]string](t, resp)["code"])
}

func TestCamelCaseRequestBodiesAreAccepted(t *testing.T) {
	h := newHarness(t, nil)
	runBackend, jobBackend := backendIDs()
	resp := h.twirp(t, "CreateArtifact", map[string]any{
		"workflowRunBackendId":    runBackend,
		"workflowJobRunBackendId": jobBackend,
		"name":                    "camel",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.True(t, decode[artifacts.CreateArtifactResponse](t, resp).OK)
}

func TestMalformedBodyIsInvalidArgument(t *testing.T) {
	h := newHarness(t, nil)
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+artifacts.TwirpPrefix+"CreateArtifact", strings.NewReader("not json"))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+h.token)
	resp, err := h.srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Equal(t, artifacts.CodeInvalidArgument, decode[map[string]string](t, resp)["code"])
}

func TestDuplicateFinalizedNameIsRefused(t *testing.T) {
	h := newHarness(t, nil)
	uploadOne(t, h, "dup", map[string]string{"a": "b"})

	runBackend, jobBackend := backendIDs()
	resp := h.twirp(t, "CreateArtifact", map[string]any{
		"workflow_run_backend_id": runBackend, "workflow_job_run_backend_id": jobBackend, "name": "dup",
	})
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.Equal(t, artifacts.CodeAlreadyExists, decode[map[string]string](t, resp)["code"])
}
