package mem

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

func cloneRunnerHost(h *model.RunnerHost) *model.RunnerHost {
	if h == nil {
		return nil
	}
	cp := *h
	cp.Labels = cloneStrings(h.Labels)
	cp.EnrolledAt = h.EnrolledAt.UTC()
	cp.ApprovedAt = h.ApprovedAt.UTC()
	cp.LastSeenAt = h.LastSeenAt.UTC()
	return &cp
}

// EnrolRunnerHost records a host the first time it is seen, and returns the
// existing row unchanged afterwards -- otherwise restarting would be a way for
// a revoked host to clear its own state.
func (s *Store) EnrolRunnerHost(_ context.Context, h *model.RunnerHost) (*model.RunnerHost, error) {
	if h == nil || h.Fingerprint == "" {
		return nil, fmt.Errorf("mem: EnrolRunnerHost: host has no fingerprint")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.hosts[h.Fingerprint]; ok {
		return cloneRunnerHost(existing), nil
	}
	cp := cloneRunnerHost(h)
	cp.State = model.RunnerHostPending
	cp.ApprovedBy, cp.ApprovedAt = "", time.Time{}
	s.hosts[h.Fingerprint] = cp
	return cloneRunnerHost(cp), nil
}

func (s *Store) GetRunnerHost(_ context.Context, fingerprint string) (*model.RunnerHost, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h, ok := s.hosts[fingerprint]
	if !ok {
		return nil, store.ErrNotFound
	}
	return cloneRunnerHost(h), nil
}

func (s *Store) ListRunnerHosts(_ context.Context) ([]*model.RunnerHost, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*model.RunnerHost, 0, len(s.hosts))
	for _, h := range s.hosts {
		out = append(out, cloneRunnerHost(h))
	}
	// Pending first: those are the ones an operator is being asked to act on.
	sort.Slice(out, func(i, j int) bool {
		if r := hostStateRank(out[i].State) - hostStateRank(out[j].State); r != 0 {
			return r < 0
		}
		if !out[i].EnrolledAt.Equal(out[j].EnrolledAt) {
			return out[i].EnrolledAt.Before(out[j].EnrolledAt)
		}
		return out[i].Fingerprint < out[j].Fingerprint
	})
	return out, nil
}

func hostStateRank(s model.RunnerHostState) int {
	switch s {
	case model.RunnerHostPending:
		return 0
	case model.RunnerHostApproved:
		return 1
	default:
		return 2
	}
}

func validRunnerHostState(s model.RunnerHostState) bool {
	switch s {
	case model.RunnerHostPending, model.RunnerHostApproved, model.RunnerHostRevoked:
		return true
	}
	return false
}

func (s *Store) SetRunnerHostState(_ context.Context, fingerprint string, state model.RunnerHostState, by, note string, at time.Time) (*model.RunnerHost, error) {
	if !validRunnerHostState(state) {
		return nil, fmt.Errorf("mem: SetRunnerHostState: invalid state %q", state)
	}
	if state == model.RunnerHostApproved && by == "" {
		return nil, fmt.Errorf("mem: SetRunnerHostState: approving a host must name who approved it")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.hosts[fingerprint]
	if !ok {
		return nil, store.ErrNotFound
	}
	h.State, h.Note = state, note
	if state == model.RunnerHostApproved {
		h.ApprovedBy, h.ApprovedAt = by, at.UTC()
	} else {
		h.ApprovedBy, h.ApprovedAt = "", time.Time{}
	}
	return cloneRunnerHost(h), nil
}

func (s *Store) SetRunnerHostLabels(_ context.Context, fingerprint string, labels []string) (*model.RunnerHost, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.hosts[fingerprint]
	if !ok {
		return nil, store.ErrNotFound
	}
	h.Labels = cloneStrings(labels)
	return cloneRunnerHost(h), nil
}

func (s *Store) TouchRunnerHost(_ context.Context, fingerprint string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.hosts[fingerprint]
	if !ok {
		return store.ErrNotFound
	}
	h.LastSeenAt = at.UTC()
	return nil
}
