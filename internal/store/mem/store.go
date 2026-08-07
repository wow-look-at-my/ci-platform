// Package mem is an in-process store for tests and for a dev instance that
// says so loudly. It is NOT durable: a restart loses every run, every job, and
// every queued piece of work.
package mem

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

// queueRow is the in-memory equivalent of the job_queue table.
type queueRow struct {
	jobID          int64
	runID          int64
	attempt        int
	labels         []string
	group          string
	priority       int
	queuedAt       time.Time
	notBefore      time.Time
	leased         bool
	runnerID       string
	leaseExpiresAt time.Time
	requeueCount   int
}

// dispatchRow is the in-memory equivalent of job_dispatches, keyed the same way.
type dispatchRow struct {
	runnerID      string
	firstAt       time.Time
	lastAt        time.Time
	dispatchCount int
}

type dispatchKey struct {
	runID   int64
	jobID   int64
	attempt int
}

type stepKey struct {
	jobID   int64
	attempt int
	number  int
}

type scopedKey struct {
	scope    string
	scopeKey string
	name     string
}

// Store is the in-memory store. Every method takes the same lock; the whole
// point is a correct reference implementation, not a fast one.
type Store struct {
	mu sync.RWMutex

	repos      map[int64]*model.Repo
	runs       map[int64]*model.Run
	jobs       map[int64]*model.Job
	steps      map[stepKey]*model.Step
	queue      map[int64]*queueRow
	dispatches map[dispatchKey]*dispatchRow
	runners    map[string]*model.Runner
	annots     map[int64][]model.Annotation
	artifacts  map[int64]*model.Artifact
	caches     map[int64]*model.CacheEntry
	cacheEvts  []model.CacheEvent
	secrets    map[scopedKey][]byte
	vars       map[scopedKey]string
	events     []store.Event
	samples    []store.QueueSample
	runNumbers map[runNumberKey]int64

	nextRun      int64
	nextJob      int64
	nextStep     int64
	nextAnnot    int64
	nextArtifact int64
	nextCache    int64
	nextCacheEvt int64
	nextEvent    int64
}

type runNumberKey struct {
	repoID       int64
	workflowPath string
}

// New builds an empty store and says loudly what it is. A control plane that
// starts on this store will forget every queued job when it restarts, so the
// warning is unconditional and cannot be turned off.
func New() *Store {
	slog.Warn("using the IN-MEMORY store: all runs, jobs, and queued work are lost on restart. " +
		"This store is unsuitable for production; point CIPLATFORM_DATABASE_URL at a file instead.")
	return &Store{
		repos:      map[int64]*model.Repo{},
		runs:       map[int64]*model.Run{},
		jobs:       map[int64]*model.Job{},
		steps:      map[stepKey]*model.Step{},
		queue:      map[int64]*queueRow{},
		dispatches: map[dispatchKey]*dispatchRow{},
		runners:    map[string]*model.Runner{},
		annots:     map[int64][]model.Annotation{},
		artifacts:  map[int64]*model.Artifact{},
		caches:     map[int64]*model.CacheEntry{},
		secrets:    map[scopedKey][]byte{},
		vars:       map[scopedKey]string{},
		runNumbers: map[runNumberKey]int64{},
	}
}

// Open exists so callers can swap mem for pg behind one constructor shape. It
// takes no DSN because there is nothing to connect to, and it warns for the
// same reason New does.
func Open(context.Context) (*Store, error) { return New(), nil }

// Durable reports false: a restart loses everything. Callers surface this in
// /healthz, which is the only reason this store is allowed to exist outside
// tests.
func (s *Store) Durable() bool { return false }

// Migrate is a no-op: the schema is Go types.
func (s *Store) Migrate(context.Context) error { return nil }

// Close is a no-op.
func (s *Store) Close() error { return nil }

var _ store.Store = (*Store)(nil)
