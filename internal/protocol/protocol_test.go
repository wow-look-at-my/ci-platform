package protocol

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/model"
)

func TestDurationMarshalsAsAReadableString(t *testing.T) {
	// The wire format is meant to be legible in a curl during an incident.
	b, err := json.Marshal(Duration(90 * time.Second))
	require.NoError(t, err)
	assert.Equal(t, `"1m30s"`, string(b))
}

func TestDurationUnmarshal(t *testing.T) {
	var d Duration
	require.NoError(t, json.Unmarshal([]byte(`"2m"`), &d))
	assert.Equal(t, 2*time.Minute, d.D())

	require.NoError(t, json.Unmarshal([]byte(`1500000000`), &d))
	assert.Equal(t, 1500*time.Millisecond, d.D())

	require.Error(t, json.Unmarshal([]byte(`"not-a-duration"`), &d))
	require.Error(t, json.Unmarshal([]byte(`abc`), &d))
}

func TestAssignmentRoundTrip(t *testing.T) {
	in := Assignment{
		RunID: 7, JobID: 11, Attempt: 2,
		IdempotencyKey: "7/11/2",
		JobName:        "publish (claude-host/agent-host, Dockerfile)",
		Steps:          []StepSpec{{Number: 1, Name: "build", Run: "make"}},
		Env:            map[string]string{"FOO": "bar"},
		SetupTimeout:   Duration(10 * time.Minute),
		Retry:          model.DefaultRetryPolicy(),
	}
	b, err := json.Marshal(in)
	require.NoError(t, err)

	var out Assignment
	require.NoError(t, json.Unmarshal(b, &out))
	assert.Equal(t, in.IdempotencyKey, out.IdempotencyKey)
	assert.Equal(t, in.JobName, out.JobName)
	assert.Equal(t, 10*time.Minute, out.SetupTimeout.D())
	require.Len(t, out.Steps, 1)
	assert.Equal(t, "make", out.Steps[0].Run)
}

// A cancellation reaches a running job through the heartbeat, and it must
// always carry its reason: there is no unexplained cancellation path.
func TestHeartbeatResponseCarriesTheCancelReason(t *testing.T) {
	in := HeartbeatResponse{Cancel: &model.CancelReason{
		Actor:    model.CancelActorConcurrencyGroup,
		Sentence: "Superseded by run 42 in concurrency group deploy-main.",
	}}
	b, err := json.Marshal(in)
	require.NoError(t, err)

	var out HeartbeatResponse
	require.NoError(t, json.Unmarshal(b, &out))
	require.NotNil(t, out.Cancel)
	require.NoError(t, out.Cancel.Validate())
	assert.Equal(t, model.CancelActorConcurrencyGroup, out.Cancel.Actor)
}

func TestCompleteRequestCarriesClassification(t *testing.T) {
	in := CompleteRequest{
		JobID: 3, Attempt: 1,
		Conclusion:        model.ConclusionInfraFailure,
		Class:             model.ClassInfra,
		ClassReason:       "registry responded 524 (Cloudflare origin timeout) -> infra",
		ClassificationLog: []string{"step 2: classified infra via rule \"cloudflare-524\""},
	}
	b, err := json.Marshal(in)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"class":"infra"`)
	assert.Contains(t, string(b), `"class_reason"`)

	var out CompleteRequest
	require.NoError(t, json.Unmarshal(b, &out))
	assert.Equal(t, model.ClassInfra, out.Class)
	require.Len(t, out.ClassificationLog, 1)
}

func TestSetupRequestBreakdown(t *testing.T) {
	in := SetupRequest{
		JobID: 1, Attempt: 1, Phase: "completed",
		Breakdown: map[string]Duration{"image_pull": Duration(45 * time.Second)},
		CacheWarm: false,
	}
	b, err := json.Marshal(in)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"image_pull":"45s"`)

	var out SetupRequest
	require.NoError(t, json.Unmarshal(b, &out))
	assert.Equal(t, 45*time.Second, out.Breakdown["image_pull"].D())
}

func TestAPIVersionIsSet(t *testing.T) {
	assert.NotEmpty(t, APIVersion)
}
