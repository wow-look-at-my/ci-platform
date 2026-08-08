package api

import (
	"strconv"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

// The DTOs below are the wire contract. They are written out by hand rather
// than reusing the model structs so that renaming a Go field never silently
// renames a JSON key a gh-alike client parses.

// TimingDTO is the phase breakdown, in seconds.
type TimingDTO struct {
	QueuedFor  float64 `json:"queued_for"`
	SetupFor   float64 `json:"setup_for"`
	ExecuteFor float64 `json:"execute_for"`
	TotalFor   float64 `json:"total_for"`
}

func timingDTO(t model.Timing) TimingDTO {
	return TimingDTO{
		QueuedFor:  t.QueuedFor.Seconds(),
		SetupFor:   t.SetupFor.Seconds(),
		ExecuteFor: t.ExecuteFor.Seconds(),
		TotalFor:   t.TotalFor.Seconds(),
	}
}

// CancelDTO is the recorded cancellation. Sentence is shown verbatim.
type CancelDTO struct {
	Actor       string `json:"actor"`
	Sentence    string `json:"sentence"`
	TriggeredBy string `json:"triggered_by,omitempty"`
}

func cancelDTO(c *model.CancelReason) *CancelDTO {
	if c == nil {
		return nil
	}
	return &CancelDTO{Actor: string(c.Actor), Sentence: c.Sentence, TriggeredBy: c.TriggeredBy}
}

// RunDTO mirrors GitHub's workflow_run plus this platform's own fields.
type RunDTO struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	WorkflowName string `json:"workflow_name"`
	WorkflowPath string `json:"workflow_path"`
	Repository   string `json:"repository"`
	RunNumber    int64  `json:"run_number"`
	RunAttempt   int    `json:"run_attempt"`
	Attempt      int    `json:"attempt"`
	Event        string `json:"event"`
	Status       string `json:"status"`
	Conclusion   string `json:"conclusion,omitempty"`
	HeadSHA      string `json:"head_sha"`
	HeadBranch   string `json:"head_branch"`
	BaseBranch   string `json:"base_branch,omitempty"`
	Actor        string `json:"actor"`
	IsForkPR     bool   `json:"is_fork_pr"`
	Approved     bool   `json:"approved"`
	ApprovedBy   string `json:"approved_by,omitempty"`

	FailureClass         string     `json:"failure_class"`
	ClassificationReason string     `json:"classification_reason,omitempty"`
	Cancel               *CancelDTO `json:"cancel,omitempty"`
	Timing               TimingDTO  `json:"timing"`

	CreatedAt   time.Time  `json:"created_at"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`

	URL     string `json:"url"`
	JobsURL string `json:"jobs_url"`
}

// runDTO renders a run. Its failure class is derived from the conclusion; the
// authoritative per-job classification lives on the jobs.
func runDTO(r *model.Run, now time.Time) RunDTO {
	d := RunDTO{
		ID:           r.ID,
		Name:         r.WorkflowName,
		WorkflowName: r.WorkflowName,
		WorkflowPath: r.WorkflowPath,
		Repository:   r.RepoFull,
		RunNumber:    r.RunNumber,
		RunAttempt:   r.Attempt,
		Attempt:      r.Attempt,
		Event:        r.Event,
		Status:       string(r.Status),
		Conclusion:   string(r.Conclusion),
		HeadSHA:      r.HeadSHA,
		HeadBranch:   r.HeadBranch,
		BaseBranch:   r.BaseBranch,
		Actor:        r.Actor,
		IsForkPR:     r.IsForkPR,
		Approved:     r.Approved,
		ApprovedBy:   r.ApprovedBy,
		FailureClass: string(classOf(r.Conclusion)),
		Cancel:       cancelDTO(r.Cancel),
		CreatedAt:    r.CreatedAt,
		StartedAt:    r.StartedAt,
		CompletedAt:  r.CompletedAt,
	}
	if r.Cancel != nil {
		d.ClassificationReason = r.Cancel.Sentence
	}
	d.Timing = timingDTO(runTiming(r, now))
	d.URL = "/api/v1/runs/" + itoa(r.ID)
	d.JobsURL = d.URL + "/jobs"
	return d
}

// classOf derives the failure class a conclusion implies. A run carries no
// class of its own; the job that failed does.
func classOf(c model.Conclusion) model.FailureClass {
	switch c {
	case model.ConclusionInfraFailure:
		return model.ClassInfra
	case model.ConclusionConfigError:
		return model.ClassConfig
	case model.ConclusionFailure, model.ConclusionTimedOut:
		return model.ClassUser
	}
	return model.ClassNone
}

// runTiming reuses the run's own timestamps. A run has no setup phase of its
// own, so SetupFor stays zero rather than being invented from job timings.
func runTiming(r *model.Run, now time.Time) model.Timing {
	end := func(p *time.Time) time.Time {
		if p != nil {
			return *p
		}
		return now
	}
	var t model.Timing
	if r.StartedAt != nil {
		t.QueuedFor = r.StartedAt.Sub(r.CreatedAt)
		t.ExecuteFor = end(r.CompletedAt).Sub(*r.StartedAt)
	} else {
		t.QueuedFor = now.Sub(r.CreatedAt)
	}
	t.TotalFor = end(r.CompletedAt).Sub(r.CreatedAt)
	for _, d := range []*time.Duration{&t.QueuedFor, &t.ExecuteFor, &t.TotalFor} {
		if *d < 0 {
			*d = 0
		}
	}
	return t
}

// StepDTO is one step of one attempt.
type StepDTO struct {
	ID              int64             `json:"id"`
	Number          int               `json:"number"`
	Name            string            `json:"name"`
	StepID          string            `json:"step_id,omitempty"`
	Status          string            `json:"status"`
	Conclusion      string            `json:"conclusion,omitempty"`
	FailureClass    string            `json:"failure_class"`
	ExitCode        int               `json:"exit_code"`
	ContinueOnError bool              `json:"continue_on_error"`
	Attempt         int               `json:"attempt"`
	StartedAt       *time.Time        `json:"started_at,omitempty"`
	CompletedAt     *time.Time        `json:"completed_at,omitempty"`
	Duration        float64           `json:"duration"`
	LogStart        int64             `json:"log_start"`
	LogEnd          int64             `json:"log_end"`
	Outputs         map[string]string `json:"outputs,omitempty"`
}

func stepDTO(s *model.Step, now time.Time) StepDTO {
	return StepDTO{
		ID:              s.ID,
		Number:          s.Number,
		Name:            s.Name,
		StepID:          s.StepID,
		Status:          string(s.Status),
		Conclusion:      string(s.Conclusion),
		FailureClass:    string(s.Class),
		ExitCode:        s.ExitCode,
		ContinueOnError: s.ContinueOnError,
		Attempt:         s.Attempt,
		StartedAt:       s.StartedAt,
		CompletedAt:     s.CompletedAt,
		Duration:        s.Duration(now).Seconds(),
		LogStart:        s.LogStart,
		LogEnd:          s.LogEnd,
		Outputs:         s.Outputs,
	}
}

// JobDTO mirrors GitHub's job plus classification, cancel reason and timing.
type JobDTO struct {
	ID        int64          `json:"id"`
	RunID     int64          `json:"run_id"`
	Key       string         `json:"key"`
	Name      string         `json:"name"`
	MatrixKey string         `json:"matrix_key,omitempty"`
	Matrix    map[string]any `json:"matrix,omitempty"`
	Needs     []string       `json:"needs"`
	Labels    []string       `json:"labels"`

	Attempt      int `json:"attempt"`
	RunAttempt   int `json:"run_attempt"`
	MaxAttempts  int `json:"max_attempts"`
	RequeueCount int `json:"requeue_count"`
	InfraRetries int `json:"infra_retry_count"`

	Status               string     `json:"status"`
	Conclusion           string     `json:"conclusion,omitempty"`
	FailureClass         string     `json:"failure_class"`
	ClassificationReason string     `json:"classification_reason,omitempty"`
	Cancel               *CancelDTO `json:"cancel,omitempty"`
	Timing               TimingDTO  `json:"timing"`

	RunnerID         string `json:"runner_id,omitempty"`
	ConcurrencyGroup string `json:"concurrency_group,omitempty"`
	ContinueOnError  bool   `json:"continue_on_error"`
	TimeoutMinutes   int    `json:"timeout_minutes,omitempty"`
	Environment      string `json:"environment,omitempty"`
	AwaitingApproval bool   `json:"awaiting_approval"`

	CreatedAt   time.Time  `json:"created_at"`
	QueuedAt    *time.Time `json:"queued_at,omitempty"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`

	Outputs map[string]string `json:"outputs,omitempty"`
	LogsURL string            `json:"logs_url"`
	URL     string            `json:"url"`
}

func jobDTO(j *model.Job, now time.Time) JobDTO {
	d := JobDTO{
		ID:                   j.ID,
		RunID:                j.RunID,
		Key:                  j.Key,
		Name:                 j.Name,
		MatrixKey:            j.MatrixKey,
		Matrix:               j.Matrix,
		Needs:                nonNil(j.Needs),
		Labels:               nonNil(j.Labels),
		Attempt:              j.Attempt,
		RunAttempt:           j.Attempt,
		MaxAttempts:          j.MaxAttempts,
		RequeueCount:         j.RequeueCount,
		InfraRetries:         j.InfraRetryCount,
		Status:               string(j.Status),
		Conclusion:           string(j.Conclusion),
		FailureClass:         string(j.Class),
		ClassificationReason: classificationReason(j),
		Cancel:               cancelDTO(j.Cancel),
		Timing:               timingDTO(j.Timing(now)),
		RunnerID:             j.RunnerID,
		ConcurrencyGroup:     j.ConcurrencyGroup,
		ContinueOnError:      j.ContinueOnError,
		TimeoutMinutes:       j.TimeoutMinutes,
		Environment:          j.Environment,
		AwaitingApproval:     j.AwaitingApproval,
		CreatedAt:            j.CreatedAt,
		QueuedAt:             j.QueuedAt,
		StartedAt:            j.StartedAt,
		CompletedAt:          j.CompletedAt,
		Outputs:              j.Outputs,
	}
	d.URL = "/api/v1/jobs/" + itoa(j.ID)
	d.LogsURL = d.URL + "/logs"
	return d
}

// classificationReason is the sentence an operator reads on the job page. The
// explanation the classifier wrote wins; the last classification-log line is
// the fallback so the field is never empty on a classified failure.
func classificationReason(j *model.Job) string {
	if j.FailureExplained != "" {
		return j.FailureExplained
	}
	if n := len(j.ClassificationLog); n > 0 {
		return j.ClassificationLog[n-1]
	}
	if j.Cancel != nil {
		return j.Cancel.Sentence
	}
	return ""
}

// JobDetailDTO is the job page: everything above plus steps, annotations and
// the audit timeline.
type JobDetailDTO struct {
	JobDTO
	RepoFull          string             `json:"repository,omitempty"`
	WorkflowName      string             `json:"workflow_name,omitempty"`
	RunNumber         int64              `json:"run_number,omitempty"`
	HeadBranch        string             `json:"head_branch,omitempty"`
	HeadSHA           string             `json:"head_sha,omitempty"`
	Steps             []StepDTO          `json:"steps"`
	Annotations       []model.Annotation `json:"annotations"`
	Events            []EventDTO         `json:"events"`
	ClassificationLog []string           `json:"classification_log,omitempty"`
}

// EventDTO is one audit-trail entry.
type EventDTO struct {
	ID      int64          `json:"id"`
	RunID   int64          `json:"run_id,omitempty"`
	JobID   int64          `json:"job_id,omitempty"`
	Kind    string         `json:"kind"`
	Message string         `json:"message"`
	Detail  map[string]any `json:"detail,omitempty"`
	At      time.Time      `json:"at"`
}

func eventDTO(e store.Event) EventDTO {
	return EventDTO{ID: e.ID, RunID: e.RunID, JobID: e.JobID, Kind: e.Kind, Message: e.Message, Detail: e.Detail, At: e.At}
}

// GraphNode is one node of the run's job DAG.
type GraphNode struct {
	ID           int64    `json:"id"`
	Key          string   `json:"key"`
	Name         string   `json:"name"`
	Status       string   `json:"status"`
	Conclusion   string   `json:"conclusion,omitempty"`
	FailureClass string   `json:"failure_class"`
	Needs        []string `json:"needs"`
	Depth        int      `json:"depth"`
}

// GraphEdge is a needs relationship, named by job key.
type GraphEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// GraphDTO is the DAG the run page draws.
type GraphDTO struct {
	Nodes []GraphNode `json:"nodes"`
	Edges []GraphEdge `json:"edges"`
}

// AttemptDTO is one entry of the run's attempt selector.
type AttemptDTO struct {
	Attempt int  `json:"attempt"`
	Current bool `json:"current"`
}

// RunDetailDTO is the run page.
type RunDetailDTO struct {
	RunDTO
	Jobs     []JobDTO     `json:"jobs"`
	Graph    GraphDTO     `json:"graph"`
	Attempts []AttemptDTO `json:"attempts"`
	Events   []EventDTO   `json:"events"`
}

// buildGraph lays the jobs out by needs depth. A cycle (which the workflow
// loader rejects long before here) leaves the unresolved nodes at the depth
// reached so far rather than looping.
func buildGraph(jobs []*model.Job) GraphDTO {
	byKey := make(map[string]*model.Job, len(jobs))
	for _, j := range jobs {
		byKey[j.Key] = j
	}
	depth := make(map[string]int, len(jobs))
	// Relax depths |jobs| times; that is enough for any acyclic graph.
	for range jobs {
		changed := false
		for _, j := range jobs {
			d := 0
			for _, n := range j.Needs {
				if _, ok := byKey[n]; !ok {
					continue
				}
				if depth[n]+1 > d {
					d = depth[n] + 1
				}
			}
			if d > depth[j.Key] {
				depth[j.Key] = d
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	g := GraphDTO{Nodes: make([]GraphNode, 0, len(jobs)), Edges: []GraphEdge{}}
	for _, j := range jobs {
		g.Nodes = append(g.Nodes, GraphNode{
			ID:           j.ID,
			Key:          j.Key,
			Name:         j.Name,
			Status:       string(j.Status),
			Conclusion:   string(j.Conclusion),
			FailureClass: string(j.Class),
			Needs:        nonNil(j.Needs),
			Depth:        depth[j.Key],
		})
		for _, n := range j.Needs {
			if _, ok := byKey[n]; ok {
				g.Edges = append(g.Edges, GraphEdge{From: n, To: j.Key})
			}
		}
	}
	return g
}

// RunnerDTO is one fleet row.
type RunnerDTO struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Labels        []string  `json:"labels"`
	Group         string    `json:"group,omitempty"`
	State         string    `json:"state"`
	CurrentJobID  int64     `json:"current_job_id,omitempty"`
	Capacity      int       `json:"capacity"`
	Version       string    `json:"version"`
	OS            string    `json:"os"`
	Arch          string    `json:"arch"`
	FirstSeenAt   time.Time `json:"first_seen_at"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
	// HeartbeatAge is seconds since the last heartbeat. The fleet page colours
	// by it, so it is computed here rather than by clock-skewed browsers.
	HeartbeatAge float64 `json:"heartbeat_age"`
}

func runnerDTO(r *model.Runner, now time.Time) RunnerDTO {
	return RunnerDTO{
		ID:            r.ID,
		Name:          r.Name,
		Labels:        nonNil(r.Labels),
		Group:         r.Group,
		State:         string(r.State),
		CurrentJobID:  r.CurrentJobID,
		Capacity:      r.Capacity,
		Version:       r.Version,
		OS:            r.OS,
		Arch:          r.Arch,
		FirstSeenAt:   r.FirstSeenAt,
		LastHeartbeat: r.LastHeartbeat,
		HeartbeatAge:  r.HeartbeatAge(now).Seconds(),
	}
}

// LogLineDTO is one log line. Seq is the deep-link anchor (#L<seq>).
type LogLineDTO struct {
	Seq    int64     `json:"seq"`
	TS     time.Time `json:"ts"`
	Step   int       `json:"step"`
	Stream string    `json:"stream"`
	Text   string    `json:"text"`
	Group  string    `json:"group,omitempty"`
}

func logLineDTO(l model.LogLine) LogLineDTO {
	return LogLineDTO{Seq: l.Seq, TS: l.Timestamp, Step: l.StepNumber, Stream: l.Stream, Text: l.Text, Group: l.Group}
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func nonNilMap[K comparable, V any](m map[K]V) map[K]V {
	if m == nil {
		return map[K]V{}
	}
	return m
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
