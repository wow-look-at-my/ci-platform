package mem

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

func (s *Store) UpsertRepo(_ context.Context, r *model.Repo) error {
	if r == nil {
		return fmt.Errorf("mem: UpsertRepo: nil repo")
	}
	if r.ID == 0 {
		return fmt.Errorf("mem: UpsertRepo: repo %q has no id", r.Owner+"/"+r.Name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repos[r.ID] = cloneRepo(r)
	return nil
}

func (s *Store) GetRepo(_ context.Context, id int64) (*model.Repo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.repos[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return cloneRepo(r), nil
}

func (s *Store) GetRepoByName(_ context.Context, owner, name string) (*model.Repo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.repos {
		if r.Owner == owner && r.Name == name {
			return cloneRepo(r), nil
		}
	}
	return nil, store.ErrNotFound
}

func (s *Store) ListRepos(_ context.Context) ([]*model.Repo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*model.Repo, 0, len(s.repos))
	for _, r := range s.repos {
		out = append(out, cloneRepo(r))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Owner != out[j].Owner {
			return out[i].Owner < out[j].Owner
		}
		return out[i].Name < out[j].Name
	})
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func (s *Store) CreateRun(_ context.Context, r *model.Run) error {
	if r == nil {
		return fmt.Errorf("mem: CreateRun: nil run")
	}
	if r.ID != 0 {
		return fmt.Errorf("mem: CreateRun: id %d already set; the store allocates ids", r.ID)
	}
	if !r.Status.Valid() {
		return fmt.Errorf("mem: CreateRun: invalid status %q", r.Status)
	}
	if r.Cancel != nil {
		if err := r.Cancel.Validate(); err != nil {
			return fmt.Errorf("mem: CreateRun: %w", err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.repos[r.RepoID]; !ok {
		return fmt.Errorf("mem: CreateRun: repo %d: %w", r.RepoID, store.ErrNotFound)
	}
	s.nextRun++
	r.ID = s.nextRun
	s.runs[r.ID] = cloneRun(r)
	return nil
}

func (s *Store) GetRun(_ context.Context, id int64) (*model.Run, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.runs[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return cloneRun(r), nil
}

func (s *Store) UpdateRun(_ context.Context, r *model.Run) error {
	if r == nil {
		return fmt.Errorf("mem: UpdateRun: nil run")
	}
	if !r.Status.Valid() {
		return fmt.Errorf("mem: UpdateRun: invalid status %q", r.Status)
	}
	if r.Cancel != nil {
		if err := r.Cancel.Validate(); err != nil {
			return fmt.Errorf("mem: UpdateRun: %w", err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.runs[r.ID]; !ok {
		return store.ErrNotFound
	}
	s.runs[r.ID] = cloneRun(r)
	return nil
}

func matchRun(r *model.Run, f store.RunFilter) bool {
	switch {
	case f.RepoID != 0 && r.RepoID != f.RepoID:
		return false
	case f.Branch != "" && r.HeadBranch != f.Branch:
		return false
	case f.Actor != "" && r.Actor != f.Actor:
		return false
	case f.Event != "" && r.Event != f.Event:
		return false
	case f.Status != "" && r.Status != f.Status:
		return false
	case f.Conclusion != "" && r.Conclusion != f.Conclusion:
		return false
	case f.Workflow != "" && r.WorkflowPath != f.Workflow && r.WorkflowName != f.Workflow:
		return false
	}
	if f.Search != "" {
		q := strings.ToLower(f.Search)
		hit := strings.Contains(strings.ToLower(r.WorkflowName), q) ||
			strings.Contains(strings.ToLower(r.HeadBranch), q) ||
			strings.Contains(strings.ToLower(r.HeadSHA), q) ||
			strings.Contains(strings.ToLower(r.Actor), q)
		if !hit {
			return false
		}
	}
	return true
}

func (s *Store) filterRuns(f store.RunFilter) []*model.Run {
	var out []*model.Run
	for _, r := range s.runs {
		if matchRun(r, f) {
			out = append(out, cloneRun(r))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out
}

func (s *Store) ListRuns(_ context.Context, f store.RunFilter) ([]*model.Run, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := s.filterRuns(f)
	if f.Offset > 0 {
		if f.Offset >= len(out) {
			return nil, nil
		}
		out = out[f.Offset:]
	}
	if f.Limit > 0 && f.Limit < len(out) {
		out = out[:f.Limit]
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func (s *Store) CountRuns(_ context.Context, f store.RunFilter) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.filterRuns(f)), nil
}

func (s *Store) ListRunsForSHA(_ context.Context, repoID int64, sha string) ([]*model.Run, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*model.Run
	for _, r := range s.runs {
		if r.RepoID == repoID && r.HeadSHA == sha {
			out = append(out, cloneRun(r))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

// NextRunNumber allocates under the store lock, so concurrent allocations
// cannot hand out the same number.
func (s *Store) NextRunNumber(_ context.Context, repoID int64, workflowPath string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := runNumberKey{repoID, workflowPath}
	s.runNumbers[k]++
	return s.runNumbers[k], nil
}

func checkJob(j *model.Job) error {
	if j == nil {
		return fmt.Errorf("nil job")
	}
	if !j.Status.Valid() {
		return fmt.Errorf("invalid status %q", j.Status)
	}
	if j.Conclusion != "" && !j.Conclusion.Valid() {
		return fmt.Errorf("invalid conclusion %q", j.Conclusion)
	}
	if !j.Class.Valid() {
		return fmt.Errorf("invalid failure class %q", j.Class)
	}
	if j.Cancel != nil {
		return j.Cancel.Validate()
	}
	return nil
}

func (s *Store) CreateJob(_ context.Context, j *model.Job) error {
	if err := checkJob(j); err != nil {
		return fmt.Errorf("mem: CreateJob: %w", err)
	}
	if j.ID != 0 {
		return fmt.Errorf("mem: CreateJob: id %d already set; the store allocates ids", j.ID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.runs[j.RunID]; !ok {
		return fmt.Errorf("mem: CreateJob: run %d: %w", j.RunID, store.ErrNotFound)
	}
	s.nextJob++
	j.ID = s.nextJob
	s.jobs[j.ID] = cloneJob(j)
	return nil
}

func (s *Store) GetJob(_ context.Context, id int64) (*model.Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j, ok := s.jobs[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return cloneJob(j), nil
}

// UpdateJob writes the whole job. A job that has reached a terminal status
// leaves the queue at the same moment: a completed job must never remain
// dispatchable.
func (s *Store) UpdateJob(_ context.Context, j *model.Job) error {
	if err := checkJob(j); err != nil {
		return fmt.Errorf("mem: UpdateJob: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.jobs[j.ID]; !ok {
		return store.ErrNotFound
	}
	s.jobs[j.ID] = cloneJob(j)
	if j.Status.Terminal() {
		delete(s.queue, j.ID)
	}
	return nil
}

func (s *Store) ListJobsForRun(_ context.Context, runID int64) ([]*model.Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*model.Job
	for _, j := range s.jobs {
		if j.RunID == runID {
			out = append(out, cloneJob(j))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *Store) ListJobsInConcurrencyGroup(_ context.Context, group string) ([]*model.Job, error) {
	if group == "" {
		return nil, fmt.Errorf("mem: ListJobsInConcurrencyGroup: empty group")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*model.Job
	for _, j := range s.jobs {
		if j.ConcurrencyGroup == group && !j.Status.Terminal() {
			out = append(out, cloneJob(j))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (s *Store) UpsertStep(_ context.Context, st *model.Step) error {
	if st == nil {
		return fmt.Errorf("mem: UpsertStep: nil step")
	}
	if st.JobID == 0 {
		return fmt.Errorf("mem: UpsertStep: step %d has no job id", st.Number)
	}
	if !st.Status.Valid() {
		return fmt.Errorf("mem: UpsertStep: invalid status %q", st.Status)
	}
	if !st.Class.Valid() {
		return fmt.Errorf("mem: UpsertStep: invalid failure class %q", st.Class)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.jobs[st.JobID]; !ok {
		return fmt.Errorf("mem: UpsertStep: job %d: %w", st.JobID, store.ErrNotFound)
	}
	k := stepKey{st.JobID, st.Attempt, st.Number}
	if prev, ok := s.steps[k]; ok {
		st.ID = prev.ID
	} else {
		s.nextStep++
		st.ID = s.nextStep
	}
	s.steps[k] = cloneStep(st)
	return nil
}

func (s *Store) ListSteps(_ context.Context, jobID int64, attempt int) ([]*model.Step, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*model.Step
	for k, st := range s.steps {
		if k.jobID == jobID && k.attempt == attempt {
			out = append(out, cloneStep(st))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}

func validRunnerState(st model.RunnerState) bool {
	switch st {
	case model.RunnerIdle, model.RunnerBusy, model.RunnerOffline, model.RunnerDrained:
		return true
	}
	return false
}

func (s *Store) RegisterRunner(_ context.Context, r *model.Runner) error {
	if r == nil {
		return fmt.Errorf("mem: RegisterRunner: nil runner")
	}
	if r.ID == "" {
		return fmt.Errorf("mem: RegisterRunner: runner has no id")
	}
	if !validRunnerState(r.State) {
		return fmt.Errorf("mem: RegisterRunner: invalid state %q", r.State)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := cloneRunner(r)
	if prev, ok := s.runners[r.ID]; ok {
		// A restarting agent is the same host, so first-seen survives.
		cp.FirstSeenAt = prev.FirstSeenAt
	}
	s.runners[r.ID] = cp
	return nil
}

func (s *Store) RunnerHeartbeat(_ context.Context, id string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runners[id]
	if !ok {
		return store.ErrNotFound
	}
	r.LastHeartbeat = at.UTC()
	if r.State == model.RunnerOffline {
		r.State = model.RunnerIdle
	}
	return nil
}

func (s *Store) GetRunner(_ context.Context, id string) (*model.Runner, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.runners[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return cloneRunner(r), nil
}

func (s *Store) ListRunners(_ context.Context) ([]*model.Runner, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*model.Runner
	for _, r := range s.runners {
		out = append(out, cloneRunner(r))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *Store) MarkOfflineRunners(_ context.Context, deadline time.Time) ([]*model.Runner, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*model.Runner
	for _, r := range s.runners {
		if r.State != model.RunnerOffline && r.LastHeartbeat.Before(deadline) {
			r.State = model.RunnerOffline
			out = append(out, cloneRunner(r))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func validAnnotationLevel(l model.AnnotationLevel) bool {
	switch l {
	case model.AnnotationNotice, model.AnnotationWarning, model.AnnotationFailure:
		return true
	}
	return false
}

func (s *Store) AddAnnotations(_ context.Context, jobID int64, as []model.Annotation) error {
	if len(as) == 0 {
		return nil
	}
	for i, a := range as {
		if !validAnnotationLevel(a.Level) {
			return fmt.Errorf("mem: AddAnnotations: annotation %d has invalid level %q", i, a.Level)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.jobs[jobID]; !ok {
		return fmt.Errorf("mem: AddAnnotations: job %d: %w", jobID, store.ErrNotFound)
	}
	for _, a := range as {
		s.nextAnnot++
		a.ID = s.nextAnnot
		a.JobID = jobID
		s.annots[jobID] = append(s.annots[jobID], a)
	}
	return nil
}

func (s *Store) ListAnnotations(_ context.Context, jobID int64) ([]model.Annotation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	src := s.annots[jobID]
	if len(src) == 0 {
		return nil, nil
	}
	return append([]model.Annotation(nil), src...), nil
}

func (s *Store) CreateArtifact(_ context.Context, a *model.Artifact) error {
	if a == nil {
		return fmt.Errorf("mem: CreateArtifact: nil artifact")
	}
	if a.ID != 0 {
		return fmt.Errorf("mem: CreateArtifact: id %d already set; the store allocates ids", a.ID)
	}
	if a.Name == "" {
		return fmt.Errorf("mem: CreateArtifact: artifact for run %d has no name", a.RunID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.runs[a.RunID]; !ok {
		return fmt.Errorf("mem: CreateArtifact: run %d: %w", a.RunID, store.ErrNotFound)
	}
	for _, existing := range s.artifacts {
		if existing.RunID == a.RunID && existing.Name == a.Name {
			return fmt.Errorf("mem: CreateArtifact: run %d already has an artifact named %q: %w",
				a.RunID, a.Name, store.ErrConflict)
		}
	}
	s.nextArtifact++
	a.ID = s.nextArtifact
	s.artifacts[a.ID] = cloneArtifact(a)
	return nil
}

func (s *Store) FinalizeArtifact(_ context.Context, id int64, size int64, digest string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.artifacts[id]
	if !ok {
		return store.ErrNotFound
	}
	now := time.Now().UTC()
	a.SizeBytes = size
	a.Digest = digest
	a.Finalized = true
	a.FinalizedAt = &now
	return nil
}

func (s *Store) GetArtifact(_ context.Context, id int64) (*model.Artifact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.artifacts[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return cloneArtifact(a), nil
}

func (s *Store) FindArtifact(_ context.Context, runID int64, name string) (*model.Artifact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, a := range s.artifacts {
		if a.RunID == runID && a.Name == name {
			return cloneArtifact(a), nil
		}
	}
	return nil, store.ErrNotFound
}

func (s *Store) ListArtifacts(_ context.Context, runID int64) ([]*model.Artifact, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*model.Artifact
	for _, a := range s.artifacts {
		if a.RunID == runID {
			out = append(out, cloneArtifact(a))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *Store) DeleteExpiredArtifacts(_ context.Context, now time.Time) ([]*model.Artifact, error) {
	s.mu.Lock()
	var out []*model.Artifact
	for id, a := range s.artifacts {
		if !a.ExpiresAt.After(now) {
			out = append(out, cloneArtifact(a))
			delete(s.artifacts, id)
		}
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > 0 {
		var bytes int64
		for _, a := range out {
			bytes += a.SizeBytes
		}
		slog.Info("deleted expired artifacts", "count", len(out), "bytes", bytes)
	}
	return out, nil
}

func (s *Store) RecordEvent(_ context.Context, e store.Event) error {
	if e.Kind == "" {
		return fmt.Errorf("mem: RecordEvent: event for run %d job %d has no kind", e.RunID, e.JobID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appendEventLocked(e)
	return nil
}

func (s *Store) appendEventLocked(e store.Event) {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	s.nextEvent++
	e.ID = s.nextEvent
	s.events = append(s.events, cloneEvent(e))
}

func (s *Store) ListEvents(_ context.Context, runID, jobID int64) ([]store.Event, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []store.Event
	for _, e := range s.events {
		if runID != 0 && e.RunID != runID {
			continue
		}
		if jobID != 0 && e.JobID != jobID {
			continue
		}
		out = append(out, cloneEvent(e))
	}
	return out, nil
}

// ArtifactUsage totals a repository's finalized artifact bytes.
func (s *Store) ArtifactUsage(_ context.Context, repoID int64) (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var total int64
	for _, a := range s.artifacts {
		if !a.Finalized {
			continue
		}
		if run, ok := s.runs[a.RunID]; ok && run.RepoID == repoID {
			total += a.SizeBytes
		}
	}
	return total, nil
}
