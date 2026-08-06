package plan

import (
	"fmt"
	"sort"
	"strings"

	"github.com/wow-look-at-my/ci-platform/internal/model"
)

// TopoSort orders job keys so every job follows everything it needs. Ties break
// on declaration order, so the order is stable across runs of the same
// workflow. A cycle is an error naming its members, never a silent drop.
func TopoSort(w *model.Workflow) ([]string, error) {
	keys := w.JobOrder
	if len(keys) != len(w.Jobs) {
		return nil, fmt.Errorf("plan: workflow defines %d jobs but the job order lists %d; declaration order is required for stable leg names",
			len(w.Jobs), len(keys))
	}
	index := make(map[string]int, len(keys))
	for i, k := range keys {
		if _, ok := w.Jobs[k]; !ok {
			return nil, fmt.Errorf("plan: job order names %q, which the workflow does not define", k)
		}
		if _, dup := index[k]; dup {
			return nil, fmt.Errorf("plan: job order names %q twice", k)
		}
		index[k] = i
	}

	indeg := make(map[string]int, len(keys))
	dependents := make(map[string][]string, len(keys))
	for _, k := range keys {
		seen := make(map[string]bool)
		for _, n := range w.Jobs[k].Needs {
			if n == k {
				return nil, fmt.Errorf("plan: job %q needs itself", k)
			}
			if _, ok := w.Jobs[n]; !ok {
				return nil, fmt.Errorf("plan: job %q needs %q, which the workflow does not define", k, n)
			}
			if seen[n] {
				continue
			}
			seen[n] = true
			indeg[k]++
			dependents[n] = append(dependents[n], k)
		}
	}

	ready := make([]string, 0, len(keys))
	for _, k := range keys {
		if indeg[k] == 0 {
			ready = append(ready, k)
		}
	}
	out := make([]string, 0, len(keys))
	for len(ready) > 0 {
		k := ready[0]
		ready = ready[1:]
		out = append(out, k)
		for _, d := range dependents[k] {
			indeg[d]--
			if indeg[d] == 0 {
				ready = append(ready, d)
			}
		}
		sort.SliceStable(ready, func(i, j int) bool { return index[ready[i]] < index[ready[j]] })
	}
	if len(out) != len(keys) {
		var stuck []string
		for _, k := range keys {
			if indeg[k] > 0 {
				stuck = append(stuck, k)
			}
		}
		return nil, fmt.Errorf("plan: needs cycle among jobs %s", strings.Join(stuck, ", "))
	}
	return out, nil
}
