package scheduler

import (
	"github.com/wow-look-at-my/ci-platform/internal/plan"
	"github.com/wow-look-at-my/ci-platform/internal/workflow/expr"
)

// realFactory is the production evaluator, the same adapter the control plane
// wires in.
func realFactory(contexts map[string]any, status plan.Status) plan.Evaluator {
	return expr.New(expr.Context(contexts)).WithStatus(expr.Status{
		Success:   status.Success,
		Failure:   status.Failure,
		Cancelled: status.Cancelled,
	})
}
