package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

// RunListDTO is the shape a gh-alike `run list` reads.
type RunListDTO struct {
	TotalCount   int      `json:"total_count"`
	Page         int      `json:"page"`
	PerPage      int      `json:"per_page"`
	WorkflowRuns []RunDTO `json:"workflow_runs"`
}

// JobListDTO is the shape a gh-alike `run view --json jobs` reads.
type JobListDTO struct {
	TotalCount int      `json:"total_count"`
	Jobs       []JobDTO `json:"jobs"`
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, err := intParam(r, "page", 1, 1, 1_000_000)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "%v", err)
		return
	}
	perPage, err := intParam(r, "per_page", 30, 1, s.cfg.MaxPerPage)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "%v", err)
		return
	}

	f := store.RunFilter{
		Branch:   strings.TrimSpace(q.Get("branch")),
		Actor:    strings.TrimSpace(q.Get("actor")),
		Event:    strings.TrimSpace(q.Get("event")),
		Workflow: strings.TrimSpace(q.Get("workflow")),
		Search:   strings.TrimSpace(q.Get("q")),
		Limit:    perPage,
		Offset:   (page - 1) * perPage,
	}
	if v := strings.TrimSpace(q.Get("status")); v != "" {
		st := model.Status(v)
		if !st.Valid() {
			writeErr(w, http.StatusBadRequest, "bad_request", "unknown status %q", v)
			return
		}
		f.Status = st
	}
	if v := strings.TrimSpace(q.Get("conclusion")); v != "" {
		c := model.Conclusion(v)
		if !c.Valid() {
			writeErr(w, http.StatusBadRequest, "bad_request", "unknown conclusion %q", v)
			return
		}
		f.Conclusion = c
	}
	if v := strings.TrimSpace(q.Get("repo")); v != "" {
		owner, name, ok := strings.Cut(v, "/")
		if !ok || owner == "" || name == "" {
			writeErr(w, http.StatusBadRequest, "bad_request", "repo filter must be owner/name, got %q", v)
			return
		}
		repo, err := s.cfg.Store.GetRepoByName(r.Context(), owner, name)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeErr(w, http.StatusNotFound, "not_found", "no such repository %q", v)
				return
			}
			storeErr(w, "get repo "+v, err)
			return
		}
		f.RepoID = repo.ID
	}

	total, err := s.cfg.Store.CountRuns(r.Context(), f)
	if err != nil {
		storeErr(w, "count runs", err)
		return
	}
	runs, err := s.cfg.Store.ListRuns(r.Context(), f)
	if err != nil {
		storeErr(w, "list runs", err)
		return
	}
	now := s.now()
	out := RunListDTO{TotalCount: total, Page: page, PerPage: perPage, WorkflowRuns: make([]RunDTO, 0, len(runs))}
	for _, run := range runs {
		out.WorkflowRuns = append(out.WorkflowRuns, runDTO(run, now))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	run, err := s.cfg.Store.GetRun(r.Context(), id)
	if err != nil {
		storeErr(w, "get run", err)
		return
	}
	jobs, err := s.cfg.Store.ListJobsForRun(r.Context(), id)
	if err != nil {
		storeErr(w, "list jobs for run", err)
		return
	}
	events, err := s.cfg.Store.ListEvents(r.Context(), id, 0)
	if err != nil {
		storeErr(w, "list run events", err)
		return
	}

	now := s.now()
	d := RunDetailDTO{RunDTO: runDTO(run, now), Jobs: make([]JobDTO, 0, len(jobs))}
	for _, j := range jobs {
		d.Jobs = append(d.Jobs, jobDTO(j, now))
	}
	d.Graph = buildGraph(jobs)
	for a := 1; a <= max(run.Attempt, 1); a++ {
		d.Attempts = append(d.Attempts, AttemptDTO{Attempt: a, Current: a == run.Attempt})
	}
	d.Events = make([]EventDTO, 0, len(events))
	for _, e := range events {
		d.Events = append(d.Events, eventDTO(e))
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) listRunJobs(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if _, err := s.cfg.Store.GetRun(r.Context(), id); err != nil {
		storeErr(w, "get run", err)
		return
	}
	jobs, err := s.cfg.Store.ListJobsForRun(r.Context(), id)
	if err != nil {
		storeErr(w, "list jobs for run", err)
		return
	}
	now := s.now()
	out := JobListDTO{TotalCount: len(jobs), Jobs: make([]JobDTO, 0, len(jobs))}
	for _, j := range jobs {
		out.Jobs = append(out.Jobs, jobDTO(j, now))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	job, err := s.cfg.Store.GetJob(r.Context(), id)
	if err != nil {
		storeErr(w, "get job", err)
		return
	}
	attempt, err := intParam(r, "attempt", job.Attempt, 1, 1000)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "%v", err)
		return
	}
	steps, err := s.cfg.Store.ListSteps(r.Context(), id, attempt)
	if err != nil {
		storeErr(w, "list steps", err)
		return
	}
	anns, err := s.cfg.Store.ListAnnotations(r.Context(), id)
	if err != nil {
		storeErr(w, "list annotations", err)
		return
	}
	events, err := s.cfg.Store.ListEvents(r.Context(), 0, id)
	if err != nil {
		storeErr(w, "list job events", err)
		return
	}

	now := s.now()
	d := JobDetailDTO{
		JobDTO:            jobDTO(job, now),
		Steps:             make([]StepDTO, 0, len(steps)),
		Annotations:       nonNil(anns),
		Events:            make([]EventDTO, 0, len(events)),
		ClassificationLog: job.ClassificationLog,
	}
	d.Attempt = attempt
	for _, st := range steps {
		d.Steps = append(d.Steps, stepDTO(st, now))
	}
	for _, e := range events {
		d.Events = append(d.Events, eventDTO(e))
	}
	// Run context so the job page can render a breadcrumb without a second
	// request. A missing run is a real inconsistency, not something to hide.
	run, err := s.cfg.Store.GetRun(r.Context(), job.RunID)
	if err != nil {
		storeErr(w, "get run for job", err)
		return
	}
	d.RepoFull = run.RepoFull
	d.WorkflowName = run.WorkflowName
	d.RunNumber = run.RunNumber
	d.HeadBranch = run.HeadBranch
	d.HeadSHA = run.HeadSHA
	writeJSON(w, http.StatusOK, d)
}
