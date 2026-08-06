package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const secret = "s3cr3t"

// recorder is a Sink that records what it was handed and can be made to fail.
type recorder struct {
	push        []*PushEvent
	pr          []*PullRequestEvent
	dispatch    []*WorkflowDispatchEvent
	runRerun    []*CheckRunEvent
	suiteRerun  []*CheckSuiteEvent
	action      []*CheckRunEvent
	install     []*InstallationEvent
	failWith    error
	lastContext context.Context
}

func (r *recorder) Push(ctx context.Context, e *PushEvent) error {
	r.lastContext, r.push = ctx, append(r.push, e)
	return r.failWith
}
func (r *recorder) PullRequest(_ context.Context, e *PullRequestEvent) error {
	r.pr = append(r.pr, e)
	return r.failWith
}
func (r *recorder) WorkflowDispatch(_ context.Context, e *WorkflowDispatchEvent) error {
	r.dispatch = append(r.dispatch, e)
	return r.failWith
}
func (r *recorder) CheckRunRerequested(_ context.Context, e *CheckRunEvent) error {
	r.runRerun = append(r.runRerun, e)
	return r.failWith
}
func (r *recorder) CheckSuiteRerequested(_ context.Context, e *CheckSuiteEvent) error {
	r.suiteRerun = append(r.suiteRerun, e)
	return r.failWith
}
func (r *recorder) RequestedAction(_ context.Context, e *CheckRunEvent) error {
	r.action = append(r.action, e)
	return r.failWith
}
func (r *recorder) Installation(_ context.Context, e *InstallationEvent) error {
	r.install = append(r.install, e)
	return r.failWith
}

func deliver(t *testing.T, h *Handler, event, body string, mutate ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set(EventHeader, event)
	req.Header.Set(DeliveryHeader, "delivery-1")
	req.Header.Set(SignatureHeader, Sign(secret, []byte(body)))
	for _, m := range mutate {
		m(req)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func newHandler(t *testing.T, opts ...Option) (*Handler, *recorder) {
	t.Helper()
	sink := &recorder{}
	opts = append(opts, WithClock(func() time.Time { return time.Unix(1700, 0).UTC() }))
	h, err := NewHandler(secret, sink, opts...)
	require.NoError(t, err)
	return h, sink
}

func TestVerify(t *testing.T) {
	body := []byte(`{"zen":"keep it logically awesome"}`)
	good := Sign(secret, body)

	require.NoError(t, Verify(secret, good, body))

	// Empty secret is a configuration error, never a skipped check.
	require.ErrorIs(t, Verify("", good, body), ErrNoSecret)

	require.ErrorIs(t, Verify(secret, "", body), ErrMissingSignature)
	require.ErrorIs(t, Verify(secret, "sha1=abcdef", body), ErrMalformedSignature)
	require.ErrorIs(t, Verify(secret, "sha256=nothex", body), ErrMalformedSignature)
	require.ErrorIs(t, Verify(secret, "sha256=ab", body), ErrMalformedSignature)
	require.ErrorIs(t, Verify(secret, Sign("other", body), body), ErrBadSignature)
	require.ErrorIs(t, Verify(secret, good, append(body, ' ')), ErrBadSignature)
}

func TestNewHandlerRejectsMissingConfig(t *testing.T) {
	_, err := NewHandler("", &recorder{})
	require.ErrorIs(t, err, ErrNoSecret)
	_, err = NewHandler(secret, nil)
	require.ErrorContains(t, err, "sink is nil")
}

func TestHandlerRejectsBadSignature(t *testing.T) {
	h, sink := newHandler(t)
	w := deliver(t, h, "push", `{}`, func(r *http.Request) {
		r.Header.Set(SignatureHeader, "sha256=00")
	})
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Empty(t, sink.push)
}

func TestHandlerRejectsNonPostAndMissingHeaders(t *testing.T) {
	h, _ := newHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/webhook", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)

	w = deliver(t, h, "", `{}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), EventHeader)

	w = deliver(t, h, "push", `not json`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestHandlerRejectsOversizeBody(t *testing.T) {
	h, _ := newHandler(t, WithMaxBody(8))
	w := deliver(t, h, "push", `{"ref":"refs/heads/main"}`)
	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
}

const pushBody = `{
	"ref":"refs/heads/main","before":"aaa","after":"bbb","created":false,"deleted":false,"forced":true,
	"head_commit":{"id":"bbb","message":"fix the thing"},
	"repository":{"id":9,"name":"ci-platform","full_name":"wow-look-at-my/ci-platform","private":true,"default_branch":"master","owner":{"id":1,"login":"wow-look-at-my","type":"Organization"}},
	"sender":{"id":2,"login":"PazerOP","type":"User"},
	"installation":{"id":4242}
}`

func TestHandlerDispatchesPushAndKeepsRawBody(t *testing.T) {
	h, sink := newHandler(t)
	w := deliver(t, h, "push", pushBody)
	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, sink.push, 1)

	e := sink.push[0]
	assert.Equal(t, "refs/heads/main", e.Ref)
	assert.Equal(t, "main", e.Branch())
	assert.Equal(t, "", e.Tag())
	assert.Equal(t, "aaa", e.Before)
	assert.Equal(t, "bbb", e.After)
	assert.True(t, e.Forced)
	assert.Equal(t, "fix the thing", e.HeadCommit.Message)
	assert.Equal(t, int64(4242), e.InstallationID)
	assert.Equal(t, "wow-look-at-my/ci-platform", e.Repo.FullName)
	assert.Equal(t, "master", e.Repo.DefaultBranch)
	assert.Equal(t, "PazerOP", e.Sender.Login)
	assert.Equal(t, "delivery-1", e.DeliveryID)
	assert.Equal(t, "push", e.Event)
	assert.Equal(t, time.Unix(1700, 0).UTC(), e.ReceivedAt)

	// The raw body survives verbatim so a re-run can rebuild the github context.
	var round map[string]any
	require.NoError(t, json.Unmarshal(e.Raw, &round))
	assert.Equal(t, "refs/heads/main", round["ref"])
	assert.JSONEq(t, pushBody, string(e.Raw))
}

func TestPushTagRef(t *testing.T) {
	h, sink := newHandler(t)
	deliver(t, h, "push", `{"ref":"refs/tags/v1.2.3"}`)
	require.Len(t, sink.push, 1)
	assert.Equal(t, "v1.2.3", sink.push[0].Tag())
	assert.Equal(t, "", sink.push[0].Branch())
}

func TestPullRequestForkDetection(t *testing.T) {
	h, sink := newHandler(t)
	body := `{"action":"opened","number":12,"pull_request":{"number":12,"draft":false,
		"head":{"ref":"feature","sha":"headsha","repo":{"id":2,"full_name":"fork/ci-platform"}},
		"base":{"ref":"master","sha":"basesha","repo":{"id":1,"full_name":"wow-look-at-my/ci-platform"}}}}`
	w := deliver(t, h, "pull_request", body)
	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, sink.pr, 1)
	e := sink.pr[0]
	assert.Equal(t, "opened", e.Action)
	assert.Equal(t, 12, e.Number)
	assert.Equal(t, "headsha", e.PullRequest.Head.SHA)
	assert.Equal(t, "master", e.PullRequest.Base.Ref)
	assert.True(t, e.IsFork())

	same := `{"action":"synchronize","pull_request":{"head":{"repo":{"id":1,"full_name":"o/r"}},"base":{"repo":{"id":1,"full_name":"o/r"}}}}`
	deliver(t, h, "pull_request", same)
	assert.False(t, sink.pr[1].IsFork())

	// Falls back to full_name when the payload omits ids.
	noIDs := `{"action":"opened","pull_request":{"head":{"repo":{"full_name":"fork/r"}},"base":{"repo":{"full_name":"o/r"}}}}`
	deliver(t, h, "pull_request", noIDs)
	assert.True(t, sink.pr[2].IsFork())
}

func TestWorkflowDispatch(t *testing.T) {
	h, sink := newHandler(t)
	body := `{"ref":"refs/heads/main","workflow":".github/workflows/ci.yml","inputs":{"level":"debug","count":"3"}}`
	w := deliver(t, h, "workflow_dispatch", body)
	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, sink.dispatch, 1)
	assert.Equal(t, ".github/workflows/ci.yml", sink.dispatch[0].Workflow)
	assert.Equal(t, "debug", sink.dispatch[0].Inputs["level"])
}

func TestCheckRunRerequestedAndRequestedAction(t *testing.T) {
	h, sink := newHandler(t)

	rerun := `{"action":"rerequested","check_run":{"id":5,"name":"build (linux)","head_sha":"sha1","external_id":"job-77","check_suite":{"id":88,"head_sha":"sha1"}}}`
	w := deliver(t, h, "check_run", rerun)
	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, sink.runRerun, 1)
	assert.Equal(t, "job-77", sink.runRerun[0].CheckRun.ExternalID)
	assert.Equal(t, int64(88), sink.runRerun[0].CheckRun.CheckSuite.ID)

	pressed := `{"action":"requested_action","requested_action":{"identifier":"rerun_failed"},"check_run":{"id":5,"external_id":"job-77"}}`
	w = deliver(t, h, "check_run", pressed)
	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, sink.action, 1)
	assert.Equal(t, "rerun_failed", sink.action[0].RequestedAction.Identifier)

	// A requested_action with no identifier is a 5xx: it is a delivery we
	// handle that we could not act on.
	w = deliver(t, h, "check_run", `{"action":"requested_action","check_run":{"id":5}}`)
	assert.Equal(t, http.StatusInternalServerError, w.Code)

	// Actions we do not act on are ignored, not errors.
	w = deliver(t, h, "check_run", `{"action":"created","check_run":{"id":5}}`)
	assert.Equal(t, http.StatusAccepted, w.Code)
	assert.Contains(t, w.Body.String(), "ignored event: check_run.created")
}

func TestCheckSuiteRerequested(t *testing.T) {
	h, sink := newHandler(t)
	w := deliver(t, h, "check_suite", `{"action":"rerequested","check_suite":{"id":3,"head_sha":"sha","head_branch":"main"}}`)
	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, sink.suiteRerun, 1)
	assert.Equal(t, int64(3), sink.suiteRerun[0].CheckSuite.ID)

	w = deliver(t, h, "check_suite", `{"action":"completed","check_suite":{"id":3}}`)
	assert.Equal(t, http.StatusAccepted, w.Code)
}

func TestInstallationEvents(t *testing.T) {
	h, sink := newHandler(t)
	body := `{"action":"created","installation":{"id":42,"account":{"login":"wow-look-at-my","type":"Organization"},"repository_selection":"selected"},
		"repositories":[{"id":1,"full_name":"o/a"}]}`
	w := deliver(t, h, "installation", body)
	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, sink.install, 1)
	assert.Equal(t, int64(42), sink.install[0].Installation.ID)
	assert.Len(t, sink.install[0].Repositories, 1)

	added := `{"action":"added","installation":{"id":42},"repositories_added":[{"id":2,"full_name":"o/b"}],"repositories_removed":[]}`
	w = deliver(t, h, "installation_repositories", added)
	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, sink.install, 2)
	assert.Equal(t, "o/b", sink.install[1].Added[0].FullName)
}

func TestUnhandledEventIsAcceptedAndLogged(t *testing.T) {
	h, sink := newHandler(t)
	w := deliver(t, h, "ping", `{"zen":"x"}`)
	assert.Equal(t, http.StatusAccepted, w.Code)
	assert.Contains(t, w.Body.String(), "ignored event: ping")
	assert.Empty(t, sink.push)

	w = deliver(t, h, "star", `{"action":"created"}`)
	assert.Equal(t, http.StatusAccepted, w.Code)
	assert.Contains(t, w.Body.String(), "ignored event: star.created")
}

func TestHandledEventFailureIsFiveHundredSoGitHubRetries(t *testing.T) {
	h, sink := newHandler(t)
	sink.failWith = errors.New("store is down")
	w := deliver(t, h, "push", pushBody)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "store is down")
}

func TestMalformedPayloadForHandledEventIsFiveHundred(t *testing.T) {
	h, _ := newHandler(t)
	// Valid JSON envelope, wrong shape for the event body.
	w := deliver(t, h, "push", `{"ref":123}`)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "decoding push payload")
}

// memDeduper is an in-test Deduper.
type memDeduper struct {
	seen map[string]bool
	err  error
}

func (m *memDeduper) Seen(_ context.Context, id string) (bool, error) {
	if m.err != nil {
		return false, m.err
	}
	if m.seen[id] {
		return true, nil
	}
	m.seen[id] = true
	return false, nil
}

func TestDeduperSuppressesRedelivery(t *testing.T) {
	d := &memDeduper{seen: map[string]bool{}}
	h, sink := newHandler(t, WithDeduper(d))

	w := deliver(t, h, "push", pushBody)
	require.Equal(t, http.StatusOK, w.Code)
	w = deliver(t, h, "push", pushBody)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "duplicate delivery delivery-1")
	assert.Len(t, sink.push, 1)
}

func TestDeduperFailureIsFiveHundred(t *testing.T) {
	d := &memDeduper{seen: map[string]bool{}, err: errors.New("db down")}
	h, sink := newHandler(t, WithDeduper(d))
	w := deliver(t, h, "push", pushBody)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Empty(t, sink.push)
}

// A nil logger or zero clock from an option must not take down the endpoint.
func TestOptionsFallBackToDefaults(t *testing.T) {
	sink := &recorder{}
	h, err := NewHandler(secret, sink, WithLogger(nil), WithClock(nil), WithMaxBody(0))
	require.NoError(t, err)
	w := deliver(t, h, "push", pushBody)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Len(t, sink.push, 1)
	assert.Equal(t, DefaultMaxBodyBytes, h.maxBodyBytes)
}

func TestTrimPrefixHelper(t *testing.T) {
	assert.Equal(t, "", trimPrefix("refs/heads/", "refs/heads/"))
	assert.Equal(t, "", trimPrefix("short", "refs/heads/"))
}
