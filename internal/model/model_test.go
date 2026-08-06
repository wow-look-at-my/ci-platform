package model

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAggregate_ZeroWorkIsNeverSuccess(t *testing.T) {
	// A required check satisfied by a job that ran nothing is the lie this
	// whole type exists to prevent.
	assert.Equal(t, ConclusionNeutral, Aggregate(nil))
	assert.Equal(t, ConclusionNeutral, Aggregate([]Conclusion{}))
}

func TestAggregate_SkippedNeverLaundersIntoSuccess(t *testing.T) {
	all := []Conclusion{ConclusionSkipped, ConclusionSkipped}
	assert.Equal(t, ConclusionSkipped, Aggregate(all))

	mixed := []Conclusion{ConclusionSuccess, ConclusionSkipped}
	assert.Equal(t, ConclusionNeutral, Aggregate(mixed))
}

func TestAggregate_ReportsTheWorstFailure(t *testing.T) {
	tests := []struct {
		name string
		in   []Conclusion
		want Conclusion
	}{
		{"all success", []Conclusion{ConclusionSuccess, ConclusionSuccess}, ConclusionSuccess},
		{"config beats infra", []Conclusion{ConclusionInfraFailure, ConclusionConfigError}, ConclusionConfigError},
		{"infra beats timeout", []Conclusion{ConclusionTimedOut, ConclusionInfraFailure}, ConclusionInfraFailure},
		{"timeout beats failure", []Conclusion{ConclusionFailure, ConclusionTimedOut}, ConclusionTimedOut},
		{"failure beats action_required", []Conclusion{ConclusionActionRequired, ConclusionFailure}, ConclusionFailure},
		{"failure beats success", []Conclusion{ConclusionSuccess, ConclusionFailure}, ConclusionFailure},
		{"failure beats cancelled", []Conclusion{ConclusionCancelled, ConclusionFailure}, ConclusionFailure},
		{"cancelled beats success", []Conclusion{ConclusionSuccess, ConclusionCancelled}, ConclusionCancelled},
		{"neutral beats success", []Conclusion{ConclusionSuccess, ConclusionNeutral}, ConclusionNeutral},
		{"stale is neutral", []Conclusion{ConclusionSuccess, ConclusionStale}, ConclusionNeutral},
		{"cancelled beats skipped", []Conclusion{ConclusionSkipped, ConclusionCancelled}, ConclusionCancelled},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Aggregate(tc.in))
		})
	}
}

func TestConclusionPredicates(t *testing.T) {
	failures := []Conclusion{
		ConclusionFailure, ConclusionTimedOut, ConclusionInfraFailure,
		ConclusionConfigError, ConclusionActionRequired,
	}
	for _, c := range failures {
		assert.True(t, c.IsFailure(), string(c))
		assert.True(t, c.Valid(), string(c))
	}
	// Skipped must not stop dependents, and must not read as a failure.
	assert.False(t, ConclusionSkipped.IsFailure())
	assert.False(t, ConclusionSuccess.IsFailure())
	assert.False(t, ConclusionCancelled.IsFailure())
	assert.False(t, Conclusion("nonsense").Valid())

	// Only a genuine build failure is red.
	assert.True(t, ConclusionFailure.UserVisibleRed())
	assert.True(t, ConclusionTimedOut.UserVisibleRed())
	assert.False(t, ConclusionInfraFailure.UserVisibleRed())
	assert.False(t, ConclusionConfigError.UserVisibleRed())
}

func TestStatus(t *testing.T) {
	for _, s := range []Status{StatusQueued, StatusInProgress, StatusCompleted, StatusWaiting} {
		assert.True(t, s.Valid(), string(s))
	}
	assert.False(t, Status("nonsense").Valid())
	assert.True(t, StatusCompleted.Terminal())
	assert.False(t, StatusQueued.Terminal())
}

func TestFailureClass(t *testing.T) {
	assert.True(t, ClassInfra.Retryable())
	assert.False(t, ClassUser.Retryable())
	assert.False(t, ClassConfig.Retryable())
	assert.False(t, ClassNone.Retryable())

	assert.Equal(t, ConclusionSuccess, ClassNone.Conclusion())
	assert.Equal(t, ConclusionFailure, ClassUser.Conclusion())
	assert.Equal(t, ConclusionInfraFailure, ClassInfra.Conclusion())
	assert.Equal(t, ConclusionConfigError, ClassConfig.Conclusion())
	assert.Equal(t, ConclusionFailure, FailureClass("bogus").Conclusion())

	for _, c := range []FailureClass{ClassNone, ClassUser, ClassInfra, ClassConfig} {
		assert.True(t, c.Valid(), string(c))
	}
	assert.False(t, FailureClass("bogus").Valid())
}

// A cancellation with no explanation is the incident this type prevents.
func TestCancelReasonValidate(t *testing.T) {
	require.NoError(t, CancelReason{Actor: CancelActorUser, Sentence: "Alex cancelled this run."}.Validate())

	err := CancelReason{Actor: CancelActorUser}.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no explanation sentence")

	err = CancelReason{Sentence: "something"}.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown actor")
}

func TestCancelActorValid(t *testing.T) {
	all := []CancelActor{
		CancelActorUser, CancelActorConcurrencyGroup, CancelActorTimeout,
		CancelActorRunnerLost, CancelActorSupersededByRun,
		CancelActorDependencyFailed, CancelActorShutdown,
	}
	for _, a := range all {
		assert.True(t, a.Valid(), string(a))
	}
	assert.False(t, CancelActor("").Valid())
}

func TestRepoFullName(t *testing.T) {
	assert.Equal(t, "wow-look-at-my/ci-platform", Repo{Owner: "wow-look-at-my", Name: "ci-platform"}.FullName())
}

func TestJobTiming(t *testing.T) {
	base := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { t := base.Add(d); return &t }

	j := Job{
		CreatedAt:        base,
		QueuedAt:         at(0),
		StartedAt:        at(30 * time.Second),
		SetupCompletedAt: at(6 * time.Minute),
		CompletedAt:      at(10 * time.Minute),
	}
	got := j.Timing(base.Add(20 * time.Minute))
	assert.Equal(t, 30*time.Second, got.QueuedFor)
	// The 5m30s setup from the incident, as a number rather than an inference.
	assert.Equal(t, 5*time.Minute+30*time.Second, got.SetupFor)
	assert.Equal(t, 4*time.Minute, got.ExecuteFor)
	assert.Equal(t, 10*time.Minute, got.TotalFor)
}

func TestJobTiming_InFlightUsesNow(t *testing.T) {
	base := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	now := base.Add(3 * time.Minute)

	queued := Job{CreatedAt: base}
	assert.Equal(t, 3*time.Minute, queued.Timing(now).QueuedFor)
	assert.Zero(t, queued.Timing(now).SetupFor)

	started := base.Add(time.Minute)
	settingUp := Job{CreatedAt: base, StartedAt: &started}
	got := settingUp.Timing(now)
	assert.Equal(t, time.Minute, got.QueuedFor)
	assert.Equal(t, 2*time.Minute, got.SetupFor)
	assert.Zero(t, got.ExecuteFor)
}

func TestJobTiming_NeverNegative(t *testing.T) {
	base := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	started := base.Add(time.Minute)
	// Clock skew between hosts must not produce a negative duration in the UI.
	j := Job{CreatedAt: base, StartedAt: &started}
	got := j.Timing(base)
	assert.GreaterOrEqual(t, got.SetupFor, time.Duration(0))
	assert.GreaterOrEqual(t, got.TotalFor, time.Duration(0))
}

func TestStepDuration(t *testing.T) {
	base := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	assert.Zero(t, Step{}.Duration(base))

	started := base
	assert.Equal(t, time.Minute, Step{StartedAt: &started}.Duration(base.Add(time.Minute)))

	done := base.Add(10 * time.Second)
	assert.Equal(t, 10*time.Second, Step{StartedAt: &started, CompletedAt: &done}.Duration(base.Add(time.Hour)))
}

func TestRunnerHeartbeatAge(t *testing.T) {
	base := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	r := Runner{LastHeartbeat: base}
	assert.Equal(t, 90*time.Second, r.HeartbeatAge(base.Add(90*time.Second)))
}

func TestExpr(t *testing.T) {
	assert.True(t, NewExpr("plain").IsLiteral())
	assert.True(t, NewExpr("").IsLiteral())
	assert.True(t, NewExpr("${").IsLiteral())
	assert.False(t, NewExpr("${{ github.sha }}").IsLiteral())
	assert.False(t, NewExpr("a-${{ x }}-b").IsLiteral())

	assert.Equal(t, "raw", NewExpr("raw").String())
	assert.True(t, NewExpr("").Empty())
	assert.False(t, NewExpr("x").Empty())
}

func TestDefaultRetryPolicy(t *testing.T) {
	p := DefaultRetryPolicy()

	// Infra retries; user and config never do, because retrying cannot help.
	assert.True(t, p.Retries(ClassInfra, 1))
	assert.True(t, p.Retries(ClassInfra, 2))
	assert.False(t, p.Retries(ClassInfra, 3), "attempts exhausted")
	assert.False(t, p.Retries(ClassUser, 1))
	assert.False(t, p.Retries(ClassConfig, 1))
}

func TestRetryPolicyDelay(t *testing.T) {
	exp := RetryPolicy{Attempts: 6, Backoff: BackoffExponential, Initial: 5 * time.Second, Max: time.Minute}
	assert.Zero(t, exp.Delay(1))
	assert.Equal(t, 5*time.Second, exp.Delay(2))
	assert.Equal(t, 10*time.Second, exp.Delay(3))
	assert.Equal(t, 20*time.Second, exp.Delay(4))
	assert.Equal(t, 40*time.Second, exp.Delay(5))
	assert.Equal(t, time.Minute, exp.Delay(6), "clamped to Max")

	fixed := RetryPolicy{Backoff: BackoffFixed, Initial: 3 * time.Second}
	assert.Equal(t, 3*time.Second, fixed.Delay(2))
	assert.Equal(t, 3*time.Second, fixed.Delay(9))

	linear := RetryPolicy{Backoff: BackoffLinear, Initial: 2 * time.Second}
	assert.Equal(t, 2*time.Second, linear.Delay(2))
	assert.Equal(t, 6*time.Second, linear.Delay(4))

	none := RetryPolicy{Backoff: BackoffNone, Initial: time.Hour}
	assert.Zero(t, none.Delay(5))

	unbounded := RetryPolicy{Backoff: BackoffExponential, Initial: time.Second}
	assert.Equal(t, 8*time.Second, unbounded.Delay(5))
}

func TestRunJSONRoundTrip(t *testing.T) {
	// The API and the UI both depend on these field names.
	b, err := json.Marshal(Run{HeadSHA: "abc", Status: StatusQueued})
	require.NoError(t, err)
	assert.Contains(t, string(b), `"head_sha":"abc"`)
	assert.Contains(t, string(b), `"status":"queued"`)
	assert.NotContains(t, string(b), `"conclusion"`, "empty conclusion is omitted")
}
