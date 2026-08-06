package mem

import (
	"context"
	"fmt"
	"sort"
	"strings"

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
