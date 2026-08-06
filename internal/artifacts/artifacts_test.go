package artifacts_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
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
	"github.com/wow-look-at-my/ci-platform/internal/blob"
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
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+artifacts.TwirpPrefix+method, bytes.NewReader(raw))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.token)
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

func TestQuotaExhaustionUsesTheMessageTheClientMatches(t *testing.T) {
	h := newHarness(t, func(o *artifacts.Options) {
		o.RepoQuotaBytes = 100
		o.RepoUsage = func(context.Context, int64) (int64, error) { return 200, nil }
	})
	runBackend, jobBackend := backendIDs()
	resp := h.twirp(t, "CreateArtifact", map[string]any{
		"workflow_run_backend_id": runBackend, "workflow_job_run_backend_id": jobBackend, "name": "over",
	})
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	body := decode[map[string]string](t, resp)
	assert.Equal(t, artifacts.CodeResourceExhausted, body["code"])
	assert.Contains(t, body["msg"], "insufficient usage",
		"UsageError.isUsageErrorMessage matches this substring to raise the quota error")
	assert.Contains(t, body["msg"], "100 byte artifact quota")
}

func TestQuotaLookupFailureIsNotTreatedAsRoom(t *testing.T) {
	h := newHarness(t, func(o *artifacts.Options) {
		o.RepoUsage = func(context.Context, int64) (int64, error) { return 0, errStoreDown }
	})
	runBackend, jobBackend := backendIDs()
	resp := h.twirp(t, "CreateArtifact", map[string]any{
		"workflow_run_backend_id": runBackend, "workflow_job_run_backend_id": jobBackend, "name": "x",
	})
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	assert.Contains(t, decode[map[string]string](t, resp)["msg"], "artifact store unavailable")
}

func TestNewRequiresAnExplicitQuotaDecision(t *testing.T) {
	fs := newFakeStore()
	bs, err := disk.New(t.TempDir())
	require.NoError(t, err)
	signer, err := jobtoken.New(jobtoken.Options{Key: []byte("0123456789abcdef0123456789abcdef"), Issuer: "https://x.localhost"})
	require.NoError(t, err)
	base := artifacts.Options{Store: fs, Blob: bs, Signer: signer, BaseURL: "https://x.localhost"}

	_, err = artifacts.New(base)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "QuotaDisabled")

	withQuota := base
	withQuota.RepoQuotaBytes = 10
	_, err = artifacts.New(withQuota)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "could never be enforced")

	disabled := base
	disabled.QuotaDisabled = true
	_, err = artifacts.New(disabled)
	assert.NoError(t, err, "running without a quota is allowed, but only on purpose")

	for _, missing := range []artifacts.Options{
		{Blob: bs, Signer: signer, BaseURL: "x", QuotaDisabled: true},
		{Store: fs, Signer: signer, BaseURL: "x", QuotaDisabled: true},
		{Store: fs, Blob: bs, BaseURL: "x", QuotaDisabled: true},
		{Store: fs, Blob: bs, Signer: signer, QuotaDisabled: true},
	} {
		_, err := artifacts.New(missing)
		assert.Error(t, err)
	}
}

func TestRetentionIsClampedAndAlwaysSet(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h := newHarness(t, func(o *artifacts.Options) {
		o.MaxRetentionDays = 7
		o.DefaultRetentionDays = 3
		o.Now = func() time.Time { return now }
	})
	runBackend, jobBackend := backendIDs()

	create := func(t *testing.T, name, expires string) *artifactRow {
		t.Helper()
		resp := h.twirp(t, "CreateArtifact", map[string]any{
			"workflow_run_backend_id": runBackend, "workflow_job_run_backend_id": jobBackend,
			"name": name, "expires_at": expires,
		})
		require.Equal(t, http.StatusOK, resp.StatusCode)
		return latestArtifact(t, h)
	}

	assert.Equal(t, now.AddDate(0, 0, 3), create(t, "default", "").ExpiresAt,
		"no requested expiry means the configured default")
	assert.Equal(t, now.AddDate(0, 0, 7), create(t, "greedy", now.AddDate(0, 0, 90).Format(time.RFC3339)).ExpiresAt,
		"a request past the maximum is clamped, not honoured")
	assert.Equal(t, now.AddDate(0, 0, 2), create(t, "short", now.AddDate(0, 0, 2).Format(time.RFC3339)).ExpiresAt,
		"a shorter request is honoured")
	assert.Equal(t, now.AddDate(0, 0, 7), create(t, "garbage", "not a timestamp").ExpiresAt,
		"an unparseable expiry falls back to the maximum, never to never")
	assert.Equal(t, now.AddDate(0, 0, 7), create(t, "past", now.AddDate(0, 0, -1).Format(time.RFC3339)).ExpiresAt)
}

type artifactRow struct {
	ExpiresAt time.Time
}

func latestArtifact(t *testing.T, h *harness) *artifactRow {
	t.Helper()
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	a := h.store.artifacts[h.store.nextID]
	require.NotNil(t, a)
	return &artifactRow{ExpiresAt: a.ExpiresAt}
}

// TestExpiryDeletesBlobsAndRecordsWhy is the "an artifact that vanished must
// have a recorded reason" case.
func TestExpiryDeletesBlobsAndRecordsWhy(t *testing.T) {
	now := time.Now()
	h := newHarness(t, func(o *artifacts.Options) {
		o.MaxRetentionDays = 1
		o.Now = func() time.Time { return now }
	})
	uploadOne(t, h, "doomed", map[string]string{"a": "b"})

	ctx := context.Background()
	_, err := h.blob.Stat(ctx, "artifacts/1/content.zip")
	require.NoError(t, err)

	// Move the clock past the retention window.
	h2 := h
	h2.svc, err = artifacts.New(artifacts.Options{
		Store: h.store, Blob: h.blob, Signer: h.signer, BaseURL: h.srv.URL,
		QuotaDisabled: true,
		Now:           func() time.Time { return now.AddDate(0, 0, 2) },
	})
	require.NoError(t, err)

	deleted, err := h2.svc.ExpireArtifacts(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, deleted)

	_, err = h.blob.Stat(ctx, "artifacts/1/content.zip")
	assert.ErrorIs(t, err, blob.ErrNotFound, "expiry deletes the bytes")

	events := h.store.eventsOfKind("artifact_expired")
	require.Len(t, events, 1)
	assert.Contains(t, events[0].Message, "doomed")
	assert.Contains(t, events[0].Message, "retention expired at")
	assert.Equal(t, int64(1), events[0].Detail["artifact_id"])
}

func TestExpiryReportsBlobFailures(t *testing.T) {
	now := time.Now()
	h := newHarness(t, func(o *artifacts.Options) { o.Now = func() time.Time { return now } })
	uploadOne(t, h, "stuck", map[string]string{"a": "b"})

	svc, err := artifacts.New(artifacts.Options{
		Store: h.store, Blob: undeletableBlob{h.blob}, Signer: h.signer, BaseURL: h.srv.URL,
		QuotaDisabled: true,
		Now:           func() time.Time { return now.AddDate(1, 0, 0) },
	})
	require.NoError(t, err)

	deleted, err := svc.ExpireArtifacts(context.Background())
	require.Error(t, err, "bytes that would not delete still count against the quota, so this cannot be silent")
	assert.Zero(t, deleted)
	assert.Contains(t, err.Error(), "delete artifact 1's content")
}

type undeletableBlob struct{ blob.Store }

func (undeletableBlob) Delete(context.Context, string) error { return errStoreDown }

func TestDeleteArtifact(t *testing.T) {
	h := newHarness(t, nil)
	uploadOne(t, h, "gone-soon", map[string]string{"a": "b"})
	runBackend, jobBackend := backendIDs()

	resp := h.twirp(t, "DeleteArtifact", map[string]any{
		"workflow_run_backend_id": runBackend, "workflow_job_run_backend_id": jobBackend, "name": "gone-soon",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "1", decode[artifacts.DeleteArtifactResponse](t, resp).ArtifactID)

	_, err := h.blob.Stat(context.Background(), "artifacts/1/content.zip")
	assert.ErrorIs(t, err, blob.ErrNotFound)
	assert.NotEmpty(t, h.store.eventsOfKind("artifact_deleted"))

	missing := h.twirp(t, "DeleteArtifact", map[string]any{
		"workflow_run_backend_id": runBackend, "workflow_job_run_backend_id": jobBackend, "name": "never-existed",
	})
	assert.Equal(t, http.StatusNotFound, missing.StatusCode)
}

func TestListSkipsUnfinalizedAndFiltersByName(t *testing.T) {
	h := newHarness(t, nil)
	uploadOne(t, h, "done", map[string]string{"a": "b"})
	runBackend, jobBackend := backendIDs()

	// Reserve a second artifact but never upload it.
	h.twirp(t, "CreateArtifact", map[string]any{
		"workflow_run_backend_id": runBackend, "workflow_job_run_backend_id": jobBackend, "name": "pending",
	})

	all := decode[artifacts.ListArtifactsResponse](t, h.twirp(t, "ListArtifacts", map[string]any{
		"workflow_run_backend_id": runBackend, "workflow_job_run_backend_id": jobBackend,
	}))
	require.Len(t, all.Artifacts, 1, "an artifact still uploading is not downloadable, so it is not listed")
	assert.Equal(t, "done", all.Artifacts[0].Name)

	byName := decode[artifacts.ListArtifactsResponse](t, h.twirp(t, "ListArtifacts", map[string]any{
		"workflow_run_backend_id": runBackend, "workflow_job_run_backend_id": jobBackend,
		"name_filter": "nothing-matches",
	}))
	assert.Empty(t, byName.Artifacts)

	badFilter := h.twirp(t, "ListArtifacts", map[string]any{
		"workflow_run_backend_id": runBackend, "workflow_job_run_backend_id": jobBackend,
		"id_filter": "not-a-number",
	})
	assert.Equal(t, http.StatusBadRequest, badFilter.StatusCode)
}

func TestGetSignedURLForUnfinishedArtifact(t *testing.T) {
	h := newHarness(t, nil)
	runBackend, jobBackend := backendIDs()
	h.twirp(t, "CreateArtifact", map[string]any{
		"workflow_run_backend_id": runBackend, "workflow_job_run_backend_id": jobBackend, "name": "pending",
	})

	resp := h.twirp(t, "GetSignedArtifactURL", map[string]any{
		"workflow_run_backend_id": runBackend, "workflow_job_run_backend_id": jobBackend, "name": "pending",
	})
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Contains(t, decode[map[string]string](t, resp)["msg"], "still uploading")
}

func TestDownloadSingleFileWithinArtifact(t *testing.T) {
	h := newHarness(t, nil)
	uploadOne(t, h, "multi", map[string]string{"out/app.txt": "the binary", "out/notes.md": "hello"})

	url, err := h.svc.DownloadURL(1)
	require.NoError(t, err)

	resp, err := http.Get(url + "&path=out/notes.md")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(body))
	assert.Contains(t, resp.Header.Get("Content-Disposition"), "notes.md")

	missing, err := http.Get(url + "&path=out/nope.txt")
	require.NoError(t, err)
	defer missing.Body.Close()
	assert.Equal(t, http.StatusNotFound, missing.StatusCode)
}

func TestDownloadRejectsUnsignedURLs(t *testing.T) {
	h := newHarness(t, nil)
	uploadOne(t, h, "guarded", map[string]string{"a": "b"})

	resp, err := http.Get(h.srv.URL + artifacts.PathDownload + "1")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)

	url, err := h.svc.DownloadURL(1)
	require.NoError(t, err)
	other := strings.Replace(url, artifacts.PathDownload+"1", artifacts.PathDownload+"2", 1)
	tampered, err := http.Get(other)
	require.NoError(t, err)
	defer tampered.Body.Close()
	assert.Equal(t, http.StatusForbidden, tampered.StatusCode)
}

func TestDownloadUnknownArtifact(t *testing.T) {
	h := newHarness(t, nil)
	url, err := h.svc.DownloadURL(404)
	require.NoError(t, err)
	resp, err := http.Get(url)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestStoreFailuresAreReported(t *testing.T) {
	h := newHarness(t, nil)
	runBackend, jobBackend := backendIDs()

	t.Run("create fails", func(t *testing.T) {
		h.store.failCreate = errStoreDown
		defer func() { h.store.failCreate = nil }()
		resp := h.twirp(t, "CreateArtifact", map[string]any{
			"workflow_run_backend_id": runBackend, "workflow_job_run_backend_id": jobBackend, "name": "x",
		})
		require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
		assert.Contains(t, decode[map[string]string](t, resp)["msg"], "artifact store unavailable")
	})

	t.Run("store returns no id", func(t *testing.T) {
		h.store.noIDOnCreate = true
		defer func() { h.store.noIDOnCreate = false }()
		resp := h.twirp(t, "CreateArtifact", map[string]any{
			"workflow_run_backend_id": runBackend, "workflow_job_run_backend_id": jobBackend, "name": "y",
		})
		require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
		assert.Contains(t, decode[map[string]string](t, resp)["msg"], "no id")
	})
}

func TestFinalizeUnknownArtifact(t *testing.T) {
	h := newHarness(t, nil)
	runBackend, jobBackend := backendIDs()
	resp := h.twirp(t, "FinalizeArtifact", map[string]any{
		"workflow_run_backend_id": runBackend, "workflow_job_run_backend_id": jobBackend,
		"name": "never-created", "size": "1",
	})
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	bad := h.twirp(t, "FinalizeArtifact", map[string]any{
		"workflow_run_backend_id": runBackend, "workflow_job_run_backend_id": jobBackend,
		"name": "x", "size": "not-a-number",
	})
	assert.Equal(t, http.StatusBadRequest, bad.StatusCode)
}

// TestValidateServerURL covers the GHES trap: @actions/artifact throws before
// its first request unless GITHUB_SERVER_URL looks like github.com, *.ghe.com,
// or *.localhost.
func TestValidateServerURL(t *testing.T) {
	for _, ok := range []string{
		"https://github.com",
		"https://ci.example.ghe.com",
		"https://ci.example.ghe.com/",
		"https://ci.localhost",
		"http://ci.localhost:8080",
		"https://CI.EXAMPLE.GHE.COM",
	} {
		assert.NoError(t, artifacts.ValidateServerURL(ok), "%s must be accepted", ok)
	}

	for _, bad := range []string{
		"https://ci.example.com",
		"https://github.example.org",
		"https://ghe.com.evil.net",
		"",
		"::not a url",
	} {
		err := artifacts.ValidateServerURL(bad)
		require.Error(t, err, "%s must be rejected", bad)
	}

	err := artifacts.ValidateServerURL("https://ci.internal.corp")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "GHESNotSupportedError",
		"the error must name the failure the operator would otherwise see in every job")
	assert.Contains(t, err.Error(), ".ghe.com")
}

func TestRunnerEnv(t *testing.T) {
	env := artifacts.RunnerEnv("https://ci.example.ghe.com/", "https://ci.example.ghe.com", 42, "tok", 30)
	assert.Equal(t, "https://ci.example.ghe.com", env[artifacts.EnvResultsURL])
	assert.Equal(t, "https://ci.example.ghe.com/", env[artifacts.EnvRuntimeURL],
		"the v3 client concatenates _apis/... onto this, so it keeps its trailing slash")
	assert.Equal(t, "tok", env[artifacts.EnvRuntimeToken])
	assert.Equal(t, "42", env[artifacts.EnvRunID])
	assert.Equal(t, "30", env[artifacts.EnvRetentionDays])
	assert.NoError(t, artifacts.ValidateServerURL(env[artifacts.EnvServerURL]))

	for _, name := range artifacts.EnvNames {
		assert.Contains(t, env, name, "EnvNames must list exactly what RunnerEnv sets")
	}
	assert.Len(t, env, len(artifacts.EnvNames))
}
