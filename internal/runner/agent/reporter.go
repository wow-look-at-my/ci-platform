package agent

import (
	"context"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/protocol"
	"github.com/wow-look-at-my/ci-platform/internal/runner/exec"
)

// reporter forwards step boundaries and annotations, anchoring each step to the
// log sequence range it produced.
type reporter struct {
	agent *Agent
	asg   *protocol.Assignment
	sink  *LogSink
}

func (r *reporter) StepStarted(ctx context.Context, spec protocol.StepSpec) error {
	return r.agent.cfg.Client.StepStart(ctx, protocol.StepStartRequest{
		RunnerID: r.agent.cfg.RunnerID,
		JobID:    r.asg.JobID,
		Attempt:  r.asg.Attempt,
		Number:   spec.Number,
		Name:     stepDisplayName(spec),
		StepID:   spec.ID,
		LogStart: r.sink.Seq() + 1,
	})
}

func (r *reporter) StepEnded(ctx context.Context, res exec.StepResult) error {
	return r.agent.cfg.Client.StepEnd(ctx, protocol.StepEndRequest{
		RunnerID:    r.agent.cfg.RunnerID,
		JobID:       r.asg.JobID,
		Attempt:     r.asg.Attempt,
		Number:      res.Number,
		Conclusion:  res.Conclusion,
		Class:       res.Class,
		ClassReason: res.ClassReason,
		ExitCode:    res.ExitCode,
		Outputs:     res.Outputs,
		LogEnd:      r.sink.Seq(),
	})
}

func (r *reporter) Annotate(ctx context.Context, anns []model.Annotation) error {
	if len(anns) == 0 {
		return nil
	}
	for i := range anns {
		anns[i].JobID = r.asg.JobID
	}
	return r.agent.cfg.Client.Annotate(ctx, protocol.AnnotateRequest{
		RunnerID:    r.agent.cfg.RunnerID,
		JobID:       r.asg.JobID,
		Attempt:     r.asg.Attempt,
		Annotations: anns,
	})
}

func stepDisplayName(spec protocol.StepSpec) string {
	switch {
	case spec.Name != "":
		return spec.Name
	case spec.Uses != "":
		return spec.Uses
	default:
		return spec.ID
	}
}
