package scheduler

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/wow-look-at-my/ci-platform/internal/model"
)

// Gating and reporting deliberately disagree, and this is where that is
// checked. Aggregate is honest about a partially-skipped matrix job; the needs
// context matches GitHub Actions so dependents still run.
func TestNeedsResult_GatingFollowsGHANotTheAggregate(t *testing.T) {
	tests := []struct {
		name string
		in   []model.Conclusion
		want string
		agg  model.Conclusion
	}{
		{
			name: "one leg skipped, one succeeded",
			in:   []model.Conclusion{model.ConclusionSuccess, model.ConclusionSkipped},
			want: "success",
			agg:  model.ConclusionNeutral,
		},
		{
			name: "every leg skipped",
			in:   []model.Conclusion{model.ConclusionSkipped, model.ConclusionSkipped},
			want: "skipped",
			agg:  model.ConclusionSkipped,
		},
		{
			name: "a failure stops dependents",
			in:   []model.Conclusion{model.ConclusionSuccess, model.ConclusionFailure},
			want: "failure",
			agg:  model.ConclusionFailure,
		},
		{
			name: "an infra failure stops dependents too",
			in:   []model.Conclusion{model.ConclusionInfraFailure},
			want: "failure",
			agg:  model.ConclusionInfraFailure,
		},
		{
			name: "cancelled",
			in:   []model.Conclusion{model.ConclusionSuccess, model.ConclusionCancelled},
			want: "cancelled",
			agg:  model.ConclusionCancelled,
		},
		{
			name: "all succeeded",
			in:   []model.Conclusion{model.ConclusionSuccess, model.ConclusionSuccess},
			want: "success",
			agg:  model.ConclusionSuccess,
		},
		{
			name: "no legs",
			in:   nil,
			want: "skipped",
			agg:  model.ConclusionNeutral,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, needsResult(tc.in), "gating")
			assert.Equal(t, tc.agg, model.Aggregate(tc.in), "reporting stays honest")
		})
	}
}
