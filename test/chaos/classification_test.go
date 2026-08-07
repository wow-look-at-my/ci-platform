// Package chaos holds one test per incident in docs/incidents.md. These are the
// product: if a killed runner stops requeuing, or a 524 stops being classified
// as infrastructure, the platform has lost the only thing it claims over
// GitHub Actions.
package chaos

import (
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/classify"
	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/test/fakes"
)

// Incident 1: a registry blob upload died with `failed: 524 : error code: 524`
// and three of six matrix legs were reported as build failures.
//
// The platform must classify it as infrastructure, retry it, and never surface
// it as a red build.
func TestIncident1_RegistryTimeoutIsInfraAndRecoversOnRetry(t *testing.T) {
	reg := fakes.NewRegistry()
	defer reg.Close()

	// Fail the first two blob pushes the way Cloudflare does, then serve.
	reg.SetFault(fakes.FaultCloudflare524, 2)

	var c classify.Classifier
	policy := model.DefaultRetryPolicy()

	var decisions []classify.Decision
	var lastStatus int
	attempt := 1
	for {
		body, status := pushBlob(t, reg.URL())
		lastStatus = status
		if status == http.StatusOK {
			break
		}

		d := c.Classify(classify.Signal{
			ExitCode:   1,
			Phase:      "run",
			Output:     fmt.Sprintf("pushing blob\nfailed: %d : %s", status, body),
			HTTPStatus: status,
			Host:       reg.Host(),
		})
		decisions = append(decisions, d)

		require.Equal(t, model.ClassInfra, d.Class,
			"a Cloudflare 524 must never be attributed to the user's code")
		require.True(t, policy.Retries(d.Class, attempt),
			"the default policy must retry an infra failure at attempt %d", attempt)

		attempt++
		require.LessOrEqual(t, attempt, policy.Attempts, "gave up before the policy said to")
	}

	assert.Equal(t, http.StatusOK, lastStatus)
	require.Len(t, decisions, 2, "both failures should have been classified")
	assert.Equal(t, int64(3), reg.BlobRequests(), "two failures plus the successful retry")

	// The decision must carry the evidence, so an operator can see why this was
	// called infrastructure rather than having to trust it.
	for _, d := range decisions {
		assert.Equal(t, "cloudflare-524", d.Rule)
		assert.Contains(t, d.Evidence, "524")
		assert.Contains(t, d.String(), "classified infra")
	}

	// And the conclusion it would produce is not the red X a broken build gets.
	assert.Equal(t, model.ConclusionInfraFailure, model.ClassInfra.Conclusion())
	assert.False(t, model.ConclusionInfraFailure.UserVisibleRed())
}

// A genuine test failure in the same shape must still be the user's, or the
// classifier has simply learned to excuse everything.
func TestIncident1_RealFailureIsStillTheUsers(t *testing.T) {
	var c classify.Classifier
	d := c.Classify(classify.Signal{
		ExitCode: 1,
		Phase:    "run",
		Output:   "--- FAIL: TestPublish (0.02s)\n    publish_test.go:41: expected 3, got 4\nFAIL",
	})
	require.Equal(t, model.ClassUser, d.Class)
	assert.False(t, d.Class.Retryable(), "a failing test must never be retried into a pass")
	assert.True(t, model.ConclusionFailure.UserVisibleRed())
}

// Incident 1, second half: the aggregate said "5/9 builds failed" without
// distinguishing the three legs the network ate. Aggregate must name the worst
// thing that happened, and infrastructure outranks a build failure so the
// headline is not "your code is broken".
func TestIncident1_AggregateNamesInfrastructureNotTheBuild(t *testing.T) {
	legs := []model.Conclusion{
		model.ConclusionSuccess,
		model.ConclusionSuccess,
		model.ConclusionSuccess,
		model.ConclusionInfraFailure,
		model.ConclusionInfraFailure,
		model.ConclusionInfraFailure,
	}
	assert.Equal(t, model.ConclusionInfraFailure, model.Aggregate(legs))
}

// Green must mean the work ran.
func TestGreenMeansTheWorkRan(t *testing.T) {
	assert.Equal(t, model.ConclusionNeutral, model.Aggregate(nil),
		"zero units of work cannot satisfy a required check")
	assert.Equal(t, model.ConclusionSkipped,
		model.Aggregate([]model.Conclusion{model.ConclusionSkipped, model.ConclusionSkipped}),
		"an all-skipped run is skipped, never success")
	assert.Equal(t, model.ConclusionNeutral,
		model.Aggregate([]model.Conclusion{model.ConclusionSuccess, model.ConclusionSkipped}),
		"a mix must not launder the skip into a green")
}

// Incident 3: an attempt went from in-progress to cancelled with no reason
// anywhere. A CancelReason cannot be built without one.
func TestIncident3_CancellationCannotBeUnexplained(t *testing.T) {
	require.Error(t, model.CancelReason{Actor: model.CancelActorTimeout}.Validate(),
		"an actor with no sentence must be rejected")
	require.Error(t, model.CancelReason{Sentence: "something happened"}.Validate(),
		"a sentence with no actor must be rejected")

	ok := model.CancelReason{
		Actor:       model.CancelActorSupersededByRun,
		Sentence:    "Superseded by run 42, which pushed a newer commit to the same branch.",
		TriggeredBy: "run/42",
	}
	require.NoError(t, ok.Validate())
}

// Incident 4: setup took 5m30s with nothing to explain it. Setup is a measured
// phase, not something inferred from adjacent timestamps.
func TestIncident4_SetupIsMeasuredNotInferred(t *testing.T) {
	base := timeAt(0)
	j := model.Job{
		CreatedAt:        base,
		QueuedAt:         ptr(timeAt(0)),
		StartedAt:        ptr(timeAt(30)),
		SetupCompletedAt: ptr(timeAt(360)),
		CompletedAt:      ptr(timeAt(600)),
	}
	tm := j.Timing(timeAt(600))
	assert.Equal(t, "30s", tm.QueuedFor.String())
	assert.Equal(t, "5m30s", tm.SetupFor.String(), "the number from the incident, reported directly")
	assert.Equal(t, "4m0s", tm.ExecuteFor.String())
}

func pushBlob(t *testing.T, base string) (string, int) {
	t.Helper()
	resp, err := http.Post(base+"/v2/img/blobs/uploads/1", "application/octet-stream", nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(body), resp.StatusCode
}
