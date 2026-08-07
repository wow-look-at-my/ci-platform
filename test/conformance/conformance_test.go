// Package conformance checks this platform's reading of a workflow against
// GitHub Actions.
//
// What it can do here: parse a corpus of real workflow files and assert that
// every one is either fully supported or explicitly refused, and that the
// resulting job names match GHA byte-for-byte. What it cannot do here is run
// the same workflow on GitHub and diff the outcomes, because that needs a
// GitHub repository and a billable runner. That half is a fixture format: a
// recorded GHA outcome goes in testdata/gha/, and the suite diffs against it.
// Files with no recording are reported as UNVERIFIED rather than counted as
// passing, because a conformance suite that scores unrun cases as green is the
// deceptive-green failure this platform exists to avoid.
package conformance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/plan"
	"github.com/wow-look-at-my/ci-platform/internal/workflow"
	"github.com/wow-look-at-my/ci-platform/internal/workflow/expr"
)

// Outcome is what this platform makes of a workflow file.
type Outcome struct {
	Path string `json:"path"`
	// Supported is false when the run would be refused with "unsupported: X".
	Supported   bool     `json:"supported"`
	Unsupported []string `json:"unsupported,omitempty"`
	// JobNames are the check-run names, which existing branch protection
	// matches on. A change here breaks somebody's merge queue.
	JobNames []string `json:"job_names,omitempty"`
	// ParseError is set when the file could not be read at all.
	ParseError string `json:"parse_error,omitempty"`
}

// GHARecording is a recorded GitHub Actions outcome for the same file.
type GHARecording struct {
	Path     string   `json:"path"`
	JobNames []string `json:"job_names"`
	// Note explains anything about how the recording was captured.
	Note string `json:"note,omitempty"`
}

func newEval(contexts map[string]any, status plan.Status) plan.Evaluator {
	return expr.New(expr.Context(contexts)).WithStatus(expr.Status{
		Success: status.Success, Failure: status.Failure, Cancelled: status.Cancelled,
	})
}

// analyse reads one workflow the way the control plane would.
func analyse(path string, src []byte) Outcome {
	out := Outcome{Path: path, Supported: true}

	w, err := workflow.Parse(path, src)
	if err != nil {
		out.Supported = false
		out.ParseError = err.Error()
		return out
	}
	for _, u := range workflow.Unsupported(w) {
		out.Supported = false
		out.Unsupported = append(out.Unsupported, u.String())
	}
	if !out.Supported {
		return out
	}

	run := &model.Run{
		ID: 1, RunNumber: 1, Attempt: 1, Event: "push",
		HeadSHA: "0123456789abcdef", HeadBranch: "main", Actor: "octocat",
		WorkflowName: w.Name, WorkflowPath: path,
	}
	p, perr := plan.Build(w, plan.Input{
		Run: run,
		Contexts: map[string]any{
			"github": map[string]any{
				"ref": "refs/heads/main", "ref_name": "main", "event_name": "push",
				"sha": run.HeadSHA, "repository": "acme/widget",
			},
			"vars":    map[string]any{},
			"secrets": map[string]any{},
			"inputs":  map[string]any{},
		},
		NewEval: newEval,
	})
	if perr != nil {
		out.Supported = false
		out.ParseError = perr.Error()
		return out
	}
	for _, j := range p.Jobs {
		out.JobNames = append(out.JobNames, j.Name)
	}
	sort.Strings(out.JobNames)
	return out
}

func corpus(t *testing.T) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	roots := []string{"testdata/workflows", "../../.github/workflows"}
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !isYAML(e.Name()) {
				continue
			}
			b, err := os.ReadFile(filepath.Join(root, e.Name()))
			require.NoError(t, err)
			files[filepath.Join(root, e.Name())] = b
		}
	}
	require.NotEmpty(t, files, "the corpus is empty, so this suite would pass by doing nothing")
	return files
}

func isYAML(name string) bool {
	return strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml")
}

// Every workflow in the corpus is either fully supported or explicitly refused.
// The state that must not exist is a third one: parsed, accepted, and quietly
// missing a key the author wrote.
func TestEveryCorpusWorkflowIsSupportedOrExplicitlyRefused(t *testing.T) {
	for path, src := range corpus(t) {
		t.Run(path, func(t *testing.T) {
			got := analyse(path, src)
			if got.Supported {
				assert.NotEmpty(t, got.JobNames, "a supported workflow must plan at least one job")
				return
			}
			// Refused is fine, as long as it says what it refused and why.
			refusal := strings.Join(append(got.Unsupported, got.ParseError), " ")
			assert.NotEmpty(t, strings.TrimSpace(refusal),
				"a refused workflow must name what it refused")
			assert.True(t,
				strings.Contains(refusal, "unsupported:") || got.ParseError != "",
				"a refusal must be an unsupported: report or a parse error, got %q", refusal)
		})
	}
}

// This platform cannot yet run its own CI, and this test pins exactly why so
// the gap is a recorded fact rather than a surprise.
//
// Our CI uses service containers for Postgres, which are Phase 2. The point of
// the test is that the refusal is explicit and names the feature and the job:
// that is the difference between "we do not support this" and a workflow that
// quietly half-runs. When service containers land, this expectation flips, and
// having to change it is the prompt to re-check docs/compatibility.md.
func TestThisRepositoryOwnWorkflowIsRefusedForANamedReason(t *testing.T) {
	src, err := os.ReadFile("../../.github/workflows/ci.yml")
	require.NoError(t, err)

	got := analyse(".github/workflows/ci.yml", src)
	require.Empty(t, got.ParseError, "our own CI must at least parse")

	require.False(t, got.Supported)

	joined := strings.Join(got.Unsupported, "\n")
	assert.Contains(t, joined, "service containers is not implemented")
	assert.Contains(t, joined, "jobs.e2e.services.postgres",
		"the refusal must name the exact job and key, not just the feature")
}

// The parts of our own CI that do not use Phase 2 features plan correctly, so
// the gap is service containers specifically and not something broader.
func TestOurOwnTestJobPlansCorrectly(t *testing.T) {
	got := analyse("ci.yml", []byte(`name: CI
on:
  push:
permissions:
  id-token: write
  contents: write
  actions: read
  checks: read
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: wow-look-at-my/go-toolchain@v1
`))
	require.Empty(t, got.ParseError)
	require.True(t, got.Supported, "%v", got.Unsupported)
	assert.Equal(t, []string{"test"}, got.JobNames)
}

// Job names are the check-run names branch protection matches on, so a change
// to how they are built breaks somebody's merge queue silently. These are the
// shapes GitHub produces.
func TestJobNamesMatchGitHub(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []string
	}{
		{
			name: "unmatrixed job uses its key",
			src: `on: push
jobs:
  build:
    runs-on: x
    steps: [{run: y}]
`,
			want: []string{"build"},
		},
		{
			name: "matrix legs carry their values in declaration order",
			src: `on: push
jobs:
  test:
    runs-on: x
    strategy:
      matrix:
        os: [linux, macos]
        go: ["1.24"]
    steps: [{run: y}]
`,
			want: []string{"test (linux, 1.24)", "test (macos, 1.24)"},
		},
		{
			// The shape the operator called out: an include-only key
			// contributes a segment, in file order.
			name: "include-only keys contribute a segment",
			src: `on: push
jobs:
  publish:
    runs-on: x
    strategy:
      matrix:
        image: [claude-host/agent-host]
        include:
          - image: claude-host/agent-host
            dockerfile: Dockerfile
    steps: [{run: y}]
`,
			want: []string{"publish (claude-host/agent-host)"},
		},
		{
			name: "an explicit name is used as-is with no suffix appended",
			src: `on: push
jobs:
  build:
    name: Build the thing
    runs-on: x
    strategy:
      matrix:
        os: [linux, macos]
    steps: [{run: y}]
`,
			want: []string{"Build the thing", "Build the thing"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := analyse("ci.yml", []byte(tc.src))
			require.Empty(t, got.ParseError)
			require.True(t, got.Supported, "%v", got.Unsupported)
			want := append([]string(nil), tc.want...)
			sort.Strings(want)
			assert.Equal(t, want, got.JobNames)
		})
	}
}

// Recorded GitHub outcomes, when present, are diffed. Absent recordings are
// reported as unverified rather than counted as passing.
func TestAgainstRecordedGitHubOutcomes(t *testing.T) {
	dir := "testdata/gha"
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		t.Skipf("no recorded GitHub outcomes in %s; run the same workflows on GitHub Actions "+
			"and save the job names there to turn this into a real diff", dir)
	}

	var verified int
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		require.NoError(t, err)

		var rec GHARecording
		require.NoError(t, json.Unmarshal(raw, &rec))

		src, err := os.ReadFile(filepath.Join("testdata/workflows", filepath.Base(rec.Path)))
		require.NoError(t, err, "recording %s names a workflow that is not in the corpus", e.Name())

		got := analyse(rec.Path, src)
		want := append([]string(nil), rec.JobNames...)
		sort.Strings(want)
		assert.Equal(t, want, got.JobNames,
			"job names diverge from the recorded GitHub outcome for %s", rec.Path)
		verified++
	}
	t.Logf("diffed %d workflow(s) against recorded GitHub outcomes", verified)
}
