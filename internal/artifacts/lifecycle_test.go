// Quota, retention, expiry, and download: an artifact that vanished must have
// a recorded reason, and a quota that silently does nothing is worse than none.
package artifacts_test

import (
	"context"
	"io"
	"net/http"
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
