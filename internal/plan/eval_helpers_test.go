package plan

import "github.com/wow-look-at-my/ci-platform/internal/workflow/expr"

// realFactory is the production evaluator, the same adapter the control plane
// wires in.
func realFactory(contexts map[string]any, status Status) Evaluator {
	return expr.New(expr.Context(contexts)).WithStatus(expr.Status{
		Success:   status.Success,
		Failure:   status.Failure,
		Cancelled: status.Cancelled,
	})
}

// realEval is realFactory over contexts with nothing failed yet.
func realEval(contexts map[string]any) Evaluator {
	return realFactory(contexts, Status{Success: true})
}
