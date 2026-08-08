package demoseed

// The five runs the demo shows, one function each. What each one is for is in docs/demo.md.

import (
	"context"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

// seedGreenRun is the ordinary case: three jobs, all of which did work.
func seedGreenRun(ctx context.Context, st store.Store, logs Logs, s *Seeded) error {
	run := &model.Run{
		WorkflowName: "CI", WorkflowPath: ".github/workflows/ci.yml",
		RunNumber: 412, Attempt: 1, Event: "push",
		HeadSHA: "9f3c1ab7e5d24680bb51c0f2a7d3e9184c6b02fd", HeadBranch: "main",
		Actor: "alex", Status: model.StatusCompleted, Conclusion: model.ConclusionSuccess,
		CreatedAt: ago(22 * time.Minute), StartedAt: agop(22 * time.Minute), CompletedAt: agop(14 * time.Minute),
	}
	if err := createRun(ctx, st, s, run); err != nil {
		return err
	}

	build := &model.Job{
		RunID: run.ID, Key: "build", Name: "build", Labels: []string{"self-hosted", "linux"},
		Attempt: 1, MaxAttempts: 3, Status: model.StatusCompleted, Conclusion: model.ConclusionSuccess,
		RunnerID: "runner-a1", Outputs: map[string]string{"version": "v1.14.2"},
		CreatedAt: ago(22 * time.Minute), QueuedAt: agop(22 * time.Minute),
		StartedAt: agop(21*time.Minute - 40*time.Second), SetupCompletedAt: agop(21 * time.Minute),
		CompletedAt: agop(18 * time.Minute),
	}
	if err := createJob(ctx, st, s, build); err != nil {
		return err
	}
	if err := steps(ctx, st, build.ID, []stepSpec{
		{"Set up job", model.ConclusionSuccess, 0, 40 * time.Second, 1, 6},
		{"actions/checkout@v4", model.ConclusionSuccess, 0, 4 * time.Second, 7, 12},
		{"make build", model.ConclusionSuccess, 0, 2*time.Minute + 51*time.Second, 13, 34},
		{"actions/upload-artifact@v4", model.ConclusionSuccess, 0, 7 * time.Second, 35, 39},
	}, ago(21*time.Minute-40*time.Second)); err != nil {
		return err
	}
	if err := appendLog(ctx, logs, build.ID, 1, buildLog); err != nil {
		return err
	}

	test := &model.Job{
		RunID: run.ID, Key: "test", Name: "test (1.25, linux)", MatrixKey: "go=1.25,os=linux",
		Matrix: map[string]any{"go": "1.25", "os": "linux"},
		Needs:  []string{"build"}, Labels: []string{"self-hosted", "linux"},
		Attempt: 1, MaxAttempts: 3, Status: model.StatusCompleted, Conclusion: model.ConclusionSuccess,
		RunnerID:  "runner-a2",
		CreatedAt: ago(22 * time.Minute), QueuedAt: agop(18 * time.Minute),
		StartedAt: agop(17*time.Minute - 50*time.Second), SetupCompletedAt: agop(17 * time.Minute),
		CompletedAt: agop(14 * time.Minute),
	}
	if err := createJob(ctx, st, s, test); err != nil {
		return err
	}
	if err := steps(ctx, st, test.ID, []stepSpec{
		{"Set up job", model.ConclusionSuccess, 0, 50 * time.Second, 1, 5},
		{"actions/checkout@v4", model.ConclusionSuccess, 0, 4 * time.Second, 6, 10},
		{"actions/cache@v4", model.ConclusionSuccess, 0, 11 * time.Second, 11, 16},
		{"go test ./...", model.ConclusionSuccess, 0, 2*time.Minute + 45*time.Second, 17, 40},
	}, ago(17*time.Minute-50*time.Second)); err != nil {
		return err
	}
	if err := appendLog(ctx, logs, test.ID, 1, shortLog("go test ./...", "Job completed: success. 4 steps ran, none skipped.")); err != nil {
		return err
	}

	lint := &model.Job{
		RunID: run.ID, Key: "lint", Name: "lint", Needs: []string{"build"},
		Labels: []string{"self-hosted", "linux"}, Attempt: 1, MaxAttempts: 3,
		Status: model.StatusCompleted, Conclusion: model.ConclusionSuccess, RunnerID: "runner-a1",
		CreatedAt: ago(22 * time.Minute), QueuedAt: agop(18 * time.Minute),
		StartedAt: agop(17 * time.Minute), SetupCompletedAt: agop(16*time.Minute - 30*time.Second),
		CompletedAt: agop(15 * time.Minute),
	}
	if err := createJob(ctx, st, s, lint); err != nil {
		return err
	}
	if err := appendLog(ctx, logs, lint.ID, 1, shortLog("golangci-lint run", "Job completed: success. 3 steps ran, none skipped.")); err != nil {
		return err
	}

	art := &model.Artifact{
		RunID: run.ID, JobID: build.ID, Name: "widget-linux-amd64",
		SizeBytes: 18_446_120, Digest: "sha256:2b1f0c8d5a9e4713bb6c02fd8471e93a5c0d6b28f41a7e93c15d0248ab73e6f1",
		StorageKey: "artifacts/1/content.zip",
		ExpiresAt:  Now.Add(90 * 24 * time.Hour), CreatedAt: ago(18 * time.Minute),
	}
	if err := st.CreateArtifact(ctx, art); err != nil {
		return err
	}
	return st.FinalizeArtifact(ctx, art.ID, art.SizeBytes, art.Digest)
}

// seedInfraRun is incident 1: a registry timeout is not a build failure. The
// job carries its classification evidence, retried on its own, and the run's
// conclusion is infra_failure rather than failure.
func seedInfraRun(ctx context.Context, st store.Store, logs Logs, s *Seeded) error {
	run := &model.Run{
		WorkflowName: "Release", WorkflowPath: ".github/workflows/release.yml",
		RunNumber: 88, Attempt: 1, Event: "push",
		HeadSHA: "4d81be09c7a2f5136e0b8d47a91c25e0fa73b8c6", HeadBranch: "main",
		Actor: "alex", Status: model.StatusCompleted, Conclusion: model.ConclusionInfraFailure,
		CreatedAt: ago(48 * time.Minute), StartedAt: agop(48 * time.Minute), CompletedAt: agop(31 * time.Minute),
	}
	if err := createRun(ctx, st, s, run); err != nil {
		return err
	}

	push := &model.Job{
		RunID: run.ID, Key: "publish", Name: "publish", Labels: []string{"self-hosted", "linux"},
		Attempt: 2, MaxAttempts: 3, Status: model.StatusCompleted,
		Conclusion: model.ConclusionInfraFailure, Class: model.ClassInfra,
		RunnerID:        "runner-b1",
		InfraRetryCount: 1,
		FailureExplained: "The registry closed the connection after 524 seconds while the image layers were " +
			"uploading. That is Cloudflare's origin timeout in front of the registry, not a result your " +
			"build produced, so this attempt is recorded as an infrastructure failure and was retried.",
		ClassificationLog: []string{
			"rule cloudflare-524 matched on step 4 output: \"error parsing HTTP 524 response body\"",
			"class=infra confident=true retry=1/3 backoff=30s",
			"attempt 2 hit the same rule; the run is reported infra_failure, not failure",
		},
		CreatedAt: ago(48 * time.Minute), QueuedAt: agop(48 * time.Minute),
		StartedAt: agop(40 * time.Minute), SetupCompletedAt: agop(39*time.Minute - 20*time.Second),
		CompletedAt: agop(31 * time.Minute),
	}
	if err := createJob(ctx, st, s, push); err != nil {
		return err
	}
	if err := steps(ctx, st, push.ID, []stepSpec{
		{"Set up job", model.ConclusionSuccess, 0, 40 * time.Second, 1, 5},
		{"actions/checkout@v4", model.ConclusionSuccess, 0, 5 * time.Second, 6, 9},
		{"docker build", model.ConclusionSuccess, 0, 3*time.Minute + 12*time.Second, 10, 22},
		{"docker push", model.ConclusionInfraFailure, 1, 8*time.Minute + 44*time.Second, 23, 33},
	}, ago(40*time.Minute)); err != nil {
		return err
	}
	if err := appendLog(ctx, logs, push.ID, 2, infraLog); err != nil {
		return err
	}
	return st.RecordEvent(ctx, store.Event{
		RunID: run.ID, JobID: push.ID, Kind: "infra_retry",
		Message: "Attempt 1 failed on an infrastructure fault (cloudflare-524) and was retried after 30s.",
		Detail:  map[string]any{"rule": "cloudflare-524", "attempt": 1, "backoff": "30s"},
		At:      ago(39 * time.Minute),
	})
}

// seedUserFailureRun is the contrast: a real test failure, coloured and
// classified as the user's, with the annotation that says which assertion.
func seedUserFailureRun(ctx context.Context, st store.Store, logs Logs, s *Seeded) error {
	run := &model.Run{
		WorkflowName: "CI", WorkflowPath: ".github/workflows/ci.yml",
		RunNumber: 411, Attempt: 1, Event: "pull_request",
		HeadSHA: "c07e5b1d93a84f2016bd7e5c8a1409fb63d2e750", HeadBranch: "fix-timeout-parsing",
		BaseBranch: "main", Actor: "sam", Status: model.StatusCompleted, Conclusion: model.ConclusionFailure,
		CreatedAt: ago(2 * time.Hour), StartedAt: agop(2 * time.Hour), CompletedAt: agop(112 * time.Minute),
	}
	if err := createRun(ctx, st, s, run); err != nil {
		return err
	}

	job := &model.Job{
		RunID: run.ID, Key: "test", Name: "test (1.25, linux)", MatrixKey: "go=1.25,os=linux",
		Matrix: map[string]any{"go": "1.25", "os": "linux"},
		Labels: []string{"self-hosted", "linux"}, Attempt: 1, MaxAttempts: 3,
		Status: model.StatusCompleted, Conclusion: model.ConclusionFailure, Class: model.ClassUser,
		RunnerID:         "runner-a2",
		FailureExplained: "go test exited 1. The failure is in your code: no infrastructure rule matched its output.",
		CreatedAt:        ago(2 * time.Hour), QueuedAt: agop(2 * time.Hour),
		StartedAt: agop(119 * time.Minute), SetupCompletedAt: agop(118 * time.Minute),
		CompletedAt: agop(112 * time.Minute),
	}
	if err := createJob(ctx, st, s, job); err != nil {
		return err
	}
	if err := steps(ctx, st, job.ID, []stepSpec{
		{"Set up job", model.ConclusionSuccess, 0, 60 * time.Second, 1, 5},
		{"actions/checkout@v4", model.ConclusionSuccess, 0, 4 * time.Second, 6, 9},
		{"go test ./...", model.ConclusionFailure, 1, 5*time.Minute + 50*time.Second, 10, 28},
	}, ago(119*time.Minute)); err != nil {
		return err
	}
	if err := appendLog(ctx, logs, job.ID, 1, userFailLog); err != nil {
		return err
	}
	return st.AddAnnotations(ctx, job.ID, []model.Annotation{{
		JobID: job.ID, Path: "internal/config/config.go", StartLine: 141, EndLine: 141,
		Level: model.AnnotationFailure, Title: "TestParseTimeout/negative",
		Message: "expected an error for \"-5s\", got a duration of -5s: a negative timeout would " +
			"make every job time out immediately.",
	}})
}

// seedRequeuedRun is incident 2: the runner disappeared mid-job. The job went
// back on the queue, kept its attempt, and finished on another runner -- it was
// never failed and never lost.
func seedRequeuedRun(ctx context.Context, st store.Store, logs Logs, s *Seeded) error {
	run := &model.Run{
		WorkflowName: "Nightly", WorkflowPath: ".github/workflows/nightly.yml",
		RunNumber: 31, Attempt: 1, Event: "schedule",
		HeadSHA: "1a7f4c0e88b2359dd6017ec4f5a92b3081de64cc", HeadBranch: "main",
		Actor: "ci", Status: model.StatusCompleted, Conclusion: model.ConclusionSuccess,
		CreatedAt: ago(5 * time.Hour), StartedAt: agop(5 * time.Hour), CompletedAt: agop(4 * time.Hour),
	}
	if err := createRun(ctx, st, s, run); err != nil {
		return err
	}

	job := &model.Job{
		RunID: run.ID, Key: "soak", Name: "soak", Labels: []string{"self-hosted", "linux"},
		Attempt: 1, MaxAttempts: 3, Status: model.StatusCompleted, Conclusion: model.ConclusionSuccess,
		RunnerID: "runner-c1", RequeueCount: 1,
		CreatedAt: ago(5 * time.Hour), QueuedAt: agop(5 * time.Hour),
		StartedAt: agop(4*time.Hour + 40*time.Minute), SetupCompletedAt: agop(4*time.Hour + 39*time.Minute),
		CompletedAt: agop(4 * time.Hour),
	}
	if err := createJob(ctx, st, s, job); err != nil {
		return err
	}
	if err := steps(ctx, st, job.ID, []stepSpec{
		{"Set up job", model.ConclusionSuccess, 0, 60 * time.Second, 1, 6},
		{"make soak", model.ConclusionSuccess, 0, 38 * time.Minute, 7, 24},
	}, ago(4*time.Hour+40*time.Minute)); err != nil {
		return err
	}
	if err := appendLog(ctx, logs, job.ID, 1, shortLog("make soak", "Job completed: success, on its second dispatch after the first runner was lost.")); err != nil {
		return err
	}
	for _, e := range []store.Event{
		{
			RunID: run.ID, JobID: job.ID, Kind: "runner_lost",
			Message: "Runner runner-b2 stopped reporting, so its lease on this job expired and the job was " +
				"put back on the queue (requeue 1). Nothing your workflow did caused this.",
			Detail: map[string]any{"actor": "runner_lost", "runner_id": "runner-b2", "requeue_count": 1},
			At:     ago(4*time.Hour + 52*time.Minute),
		},
		{
			RunID: run.ID, JobID: job.ID, Kind: "redispatched",
			Message: "Redispatched to runner runner-c1 after the previous lease was lost.",
			Detail:  map[string]any{"runner_id": "runner-c1", "attempt": 1},
			At:      ago(4*time.Hour + 41*time.Minute),
		},
	} {
		if err := st.RecordEvent(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

// seedCancelledRun is incident 3: nothing is cancelled without a reason. The
// actor and the sentence are stored on the run and shown wherever it appears.
func seedCancelledRun(ctx context.Context, st store.Store, logs Logs, s *Seeded) error {
	reason := model.CancelReason{
		Actor: model.CancelActorConcurrencyGroup,
		Sentence: "Superseded by run #412 on the same branch: the workflow's concurrency group " +
			"ci-main allows one run at a time and cancel-in-progress is set.",
		TriggeredBy: "ci-main",
	}
	run := &model.Run{
		WorkflowName: "CI", WorkflowPath: ".github/workflows/ci.yml",
		RunNumber: 410, Attempt: 1, Event: "push",
		HeadSHA: "77b0e2c419d8a53f6b1c04e29d7538ab60f1ca94", HeadBranch: "main",
		Actor: "alex", Status: model.StatusCompleted, Conclusion: model.ConclusionCancelled,
		Cancel:    &reason,
		CreatedAt: ago(26 * time.Minute), StartedAt: agop(26 * time.Minute), CompletedAt: agop(23 * time.Minute),
	}
	if err := createRun(ctx, st, s, run); err != nil {
		return err
	}

	job := &model.Job{
		RunID: run.ID, Key: "build", Name: "build", Labels: []string{"self-hosted", "linux"},
		Attempt: 1, MaxAttempts: 3, Status: model.StatusCompleted,
		Conclusion: model.ConclusionCancelled, Cancel: &reason,
		ConcurrencyGroup: "ci-main", CancelInProgress: true, RunnerID: "runner-a1",
		CreatedAt: ago(26 * time.Minute), QueuedAt: agop(26 * time.Minute),
		StartedAt: agop(25 * time.Minute), SetupCompletedAt: agop(24*time.Minute - 30*time.Second),
		CompletedAt: agop(23 * time.Minute),
	}
	if err := createJob(ctx, st, s, job); err != nil {
		return err
	}
	if err := appendLog(ctx, logs, job.ID, 1, cancelledLog); err != nil {
		return err
	}
	return st.RecordEvent(ctx, store.Event{
		RunID: run.ID, JobID: job.ID, Kind: "cancelled",
		Message: reason.Sentence,
		Detail:  map[string]any{"actor": string(reason.Actor), "triggered_by": reason.TriggeredBy},
		At:      ago(23 * time.Minute),
	})
}
