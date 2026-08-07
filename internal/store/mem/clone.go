package mem

import (
	"encoding/json"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/store"
)

// Everything crossing the store boundary is deep-copied, in both directions.
// Handing a caller a pointer into the map lets an unrelated goroutine mutate
// stored state by accident, which is a bug that only ever shows up in
// production; returning a copy makes that impossible.
//
// Empty collections normalize to nil so a value round-trips through this store
// and the SQLite one identically.

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func cloneStrings(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return append([]string(nil), s...)
}

func cloneStringMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func cloneIntMap(m map[string]int) map[string]int {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// cloneAnyMap round-trips through JSON. The values are arbitrary decoded YAML or
// JSON, so a shallow copy would still share nested maps and slices.
func cloneAnyMap(m map[string]any) map[string]any {
	if len(m) == 0 {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		// The same values are stored as JSON text in SQLite, where this is a hard
		// write error. Panicking keeps the two stores honest about what they
		// accept rather than quietly storing a value pg would reject.
		panic("mem: value is not JSON-encodable, and the SQLite store would reject it: " + err.Error())
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		panic("mem: value did not survive a JSON round-trip: " + err.Error())
	}
	return out
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	return append([]byte(nil), b...)
}

func cloneCancel(c *model.CancelReason) *model.CancelReason {
	if c == nil {
		return nil
	}
	cp := *c
	return &cp
}

func cloneRepo(r *model.Repo) *model.Repo {
	if r == nil {
		return nil
	}
	cp := *r
	return &cp
}

func cloneRun(r *model.Run) *model.Run {
	if r == nil {
		return nil
	}
	cp := *r
	cp.Cancel = cloneCancel(r.Cancel)
	cp.Inputs = cloneAnyMap(r.Inputs)
	cp.EventPayload = json.RawMessage(cloneBytes(r.EventPayload))
	cp.CreatedAt = r.CreatedAt.UTC()
	cp.StartedAt = cloneTime(r.StartedAt)
	cp.CompletedAt = cloneTime(r.CompletedAt)
	return &cp
}

func cloneJob(j *model.Job) *model.Job {
	if j == nil {
		return nil
	}
	cp := *j
	cp.Matrix = cloneAnyMap(j.Matrix)
	cp.Needs = cloneStrings(j.Needs)
	cp.Labels = cloneStrings(j.Labels)
	cp.Outputs = cloneStringMap(j.Outputs)
	cp.ClassificationLog = cloneStrings(j.ClassificationLog)
	cp.Cancel = cloneCancel(j.Cancel)
	cp.CreatedAt = j.CreatedAt.UTC()
	cp.QueuedAt = cloneTime(j.QueuedAt)
	cp.StartedAt = cloneTime(j.StartedAt)
	cp.SetupCompletedAt = cloneTime(j.SetupCompletedAt)
	cp.CompletedAt = cloneTime(j.CompletedAt)
	cp.LeaseExpiresAt = cloneTime(j.LeaseExpiresAt)
	cp.LastHeartbeatAt = cloneTime(j.LastHeartbeatAt)
	return &cp
}

func cloneStep(s *model.Step) *model.Step {
	if s == nil {
		return nil
	}
	cp := *s
	cp.Outputs = cloneStringMap(s.Outputs)
	cp.StartedAt = cloneTime(s.StartedAt)
	cp.CompletedAt = cloneTime(s.CompletedAt)
	return &cp
}

func cloneRunner(r *model.Runner) *model.Runner {
	if r == nil {
		return nil
	}
	cp := *r
	cp.Labels = cloneStrings(r.Labels)
	cp.FirstSeenAt = r.FirstSeenAt.UTC()
	cp.LastHeartbeat = r.LastHeartbeat.UTC()
	return &cp
}

func cloneArtifact(a *model.Artifact) *model.Artifact {
	if a == nil {
		return nil
	}
	cp := *a
	cp.CreatedAt = a.CreatedAt.UTC()
	cp.ExpiresAt = a.ExpiresAt.UTC()
	cp.FinalizedAt = cloneTime(a.FinalizedAt)
	return &cp
}

func cloneCacheEntry(e *model.CacheEntry) *model.CacheEntry {
	if e == nil {
		return nil
	}
	cp := *e
	cp.CreatedAt = e.CreatedAt.UTC()
	cp.LastAccessed = e.LastAccessed.UTC()
	return &cp
}

func cloneEvent(e store.Event) store.Event {
	cp := e
	cp.Detail = cloneAnyMap(e.Detail)
	cp.At = e.At.UTC()
	return cp
}

func cloneQueueSample(s store.QueueSample) store.QueueSample {
	cp := s
	cp.DepthByLabel = cloneIntMap(s.DepthByLabel)
	cp.At = s.At.UTC()
	return cp
}
