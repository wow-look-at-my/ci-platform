package artifacts_test

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

// fakeStore implements the narrow artifacts.Store: the Artifacts and Events
// slices of store.Store, and nothing else.
type fakeStore struct {
	mu        sync.Mutex
	nextID    int64
	artifacts map[int64]*model.Artifact
	events    []store.Event

	// failCreate, failFinalize, and failEvent force the error paths.
	failCreate   error
	failFinalize error
	failEvent    error
	// noIDOnCreate reproduces a store that forgets to assign an id.
	noIDOnCreate bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{artifacts: map[int64]*model.Artifact{}}
}

func (f *fakeStore) CreateArtifact(_ context.Context, a *model.Artifact) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failCreate != nil {
		return f.failCreate
	}
	if f.noIDOnCreate {
		return nil
	}
	f.nextID++
	a.ID = f.nextID
	copied := *a
	f.artifacts[a.ID] = &copied
	return nil
}

func (f *fakeStore) FinalizeArtifact(_ context.Context, id, size int64, digest string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failFinalize != nil {
		return f.failFinalize
	}
	a, ok := f.artifacts[id]
	if !ok {
		return store.ErrNotFound
	}
	now := time.Now()
	a.SizeBytes, a.Digest, a.Finalized, a.FinalizedAt = size, digest, true, &now
	return nil
}

func (f *fakeStore) GetArtifact(_ context.Context, id int64) (*model.Artifact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.artifacts[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	copied := *a
	return &copied, nil
}

func (f *fakeStore) FindArtifact(_ context.Context, runID int64, name string) (*model.Artifact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.artifacts {
		if a.RunID == runID && a.Name == name {
			copied := *a
			return &copied, nil
		}
	}
	return nil, store.ErrNotFound
}

func (f *fakeStore) ListArtifacts(_ context.Context, runID int64) ([]*model.Artifact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*model.Artifact
	for id := int64(1); id <= f.nextID; id++ {
		if a, ok := f.artifacts[id]; ok && a.RunID == runID {
			copied := *a
			out = append(out, &copied)
		}
	}
	return out, nil
}

func (f *fakeStore) DeleteExpiredArtifacts(_ context.Context, now time.Time) ([]*model.Artifact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*model.Artifact
	for id, a := range f.artifacts {
		if a.ExpiresAt.Before(now) {
			copied := *a
			out = append(out, &copied)
			delete(f.artifacts, id)
		}
	}
	return out, nil
}

func (f *fakeStore) RecordEvent(_ context.Context, e store.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failEvent != nil {
		return f.failEvent
	}
	f.events = append(f.events, e)
	return nil
}

func (f *fakeStore) ListEvents(_ context.Context, runID, jobID int64) ([]store.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.Event
	for _, e := range f.events {
		if (runID == 0 || e.RunID == runID) && (jobID == 0 || e.JobID == jobID) {
			out = append(out, e)
		}
	}
	return out, nil
}

// eventsOfKind returns every recorded event with a kind.
func (f *fakeStore) eventsOfKind(kind string) []store.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.Event
	for _, e := range f.events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

var errStoreDown = errors.New("artifact store unavailable")
