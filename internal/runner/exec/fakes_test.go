package exec

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/wow-look-at-my/ci-platform/internal/model"
	"github.com/wow-look-at-my/ci-platform/internal/protocol"
	"github.com/wow-look-at-my/ci-platform/internal/runner/actions"
)

// fakeSandbox is an in-memory stand-in for the DinD container, so the executor
// is exercised without docker.
type fakeSandbox struct {
	mu       sync.Mutex
	files    map[string][]byte
	dirs     map[string]bool
	copies   map[string]string
	binaries map[string]string
	runs     []RunRequest
	// handle decides what each command does; nil means exit 0 silently.
	handle func(sb *fakeSandbox, req RunRequest) (RunResult, error)
}

func newFakeSandbox() *fakeSandbox {
	return &fakeSandbox{
		files:    map[string][]byte{},
		dirs:     map[string]bool{},
		copies:   map[string]string{},
		binaries: map[string]string{"bash": "/bin/bash", "sh": "/bin/sh"},
	}
}

func (s *fakeSandbox) Run(_ context.Context, req RunRequest) (RunResult, error) {
	s.mu.Lock()
	s.runs = append(s.runs, req)
	h := s.handle
	s.mu.Unlock()
	if h == nil {
		return RunResult{}, nil
	}
	return h(s, req)
}

func (s *fakeSandbox) WriteFile(_ context.Context, path string, data []byte, _ fs.FileMode) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[path] = append([]byte(nil), data...)
	return nil
}

func (s *fakeSandbox) ReadFile(_ context.Context, path string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.files[path]
	if !ok {
		return nil, fmt.Errorf("no such file %s", path)
	}
	return b, nil
}

func (s *fakeSandbox) MkdirAll(_ context.Context, dir string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dirs[dir] = true
	return nil
}

func (s *fakeSandbox) RemoveAll(_ context.Context, path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.files, path)
	return nil
}

func (s *fakeSandbox) CopyInto(_ context.Context, hostDir, containerPath string) error {
	s.mu.Lock()
	s.copies[containerPath] = hostDir
	s.dirs[containerPath] = true
	s.mu.Unlock()
	// Mirror the host directory into the fake filesystem so metadata reads and
	// entrypoint paths behave like the real thing.
	return filepath.Walk(hostDir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, rerr := filepath.Rel(hostDir, p)
		if rerr != nil {
			return rerr
		}
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		s.mu.Lock()
		s.files[containerPath+"/"+filepath.ToSlash(rel)] = data
		s.mu.Unlock()
		return nil
	})
}

func (s *fakeSandbox) LookPath(_ context.Context, bin string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.binaries[bin]; ok {
		return p, nil
	}
	return "", fmt.Errorf("%q is not installed in the sandbox image", bin)
}

// script returns the script written for the most recent run of the given shell.
func (s *fakeSandbox) script(path string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.files[path])
}

// write simulates a step writing to one of its env files.
func (s *fakeSandbox) write(path, content string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[path] = []byte(content)
}

// recordingLog collects everything the executor logs.
type recordingLog struct {
	mu    sync.Mutex
	lines []loggedLine
}

type loggedLine struct {
	step   int
	stream string
	group  string
	text   string
}

func (l *recordingLog) Line(step int, stream, group, text string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, loggedLine{step, stream, group, text})
}

func (l *recordingLog) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var b strings.Builder
	for _, ln := range l.lines {
		b.WriteString(ln.text)
		b.WriteString("\n")
	}
	return b.String()
}

func (l *recordingLog) contains(sub string) bool { return strings.Contains(l.text(), sub) }

func (l *recordingLog) count(sub string) int { return strings.Count(l.text(), sub) }

// recordingReporter captures step boundaries and annotations.
type recordingReporter struct {
	started []protocol.StepSpec
	ended   []StepResult
	annots  []model.Annotation
	err     error
}

func (r *recordingReporter) StepStarted(_ context.Context, s protocol.StepSpec) error {
	r.started = append(r.started, s)
	return r.err
}

func (r *recordingReporter) StepEnded(_ context.Context, res StepResult) error {
	r.ended = append(r.ended, res)
	return r.err
}

func (r *recordingReporter) Annotate(_ context.Context, a []model.Annotation) error {
	r.annots = append(r.annots, a...)
	return r.err
}

// fakeEvaluator understands just enough expression syntax to drive the
// executor's decisions: status functions, literals, and dotted context lookups.
type fakeEvaluator struct {
	contexts map[string]any
	status   Status
	fail     map[string]bool
}

func newFakeEvaluatorFactory(fail map[string]bool) EvaluatorFactory {
	return func(contexts map[string]any, status Status) Evaluator {
		return &fakeEvaluator{contexts: contexts, status: status, fail: fail}
	}
}

func (f *fakeEvaluator) EvalBool(expr string) (bool, error) {
	expr = strings.TrimSpace(expr)
	if f.fail[expr] {
		return false, fmt.Errorf("unrecognized named-value %q", expr)
	}
	switch expr {
	case "true", "always()":
		return true, nil
	case "false":
		return false, nil
	case "success()":
		return f.status.Success, nil
	case "failure()":
		return f.status.Failure, nil
	case "cancelled()":
		return f.status.Cancelled, nil
	}
	v, err := f.lookup(expr)
	if err != nil {
		return false, err
	}
	return v != "" && v != "false", nil
}

func (f *fakeEvaluator) EvalString(s string) (string, error) {
	out := s
	for {
		start := strings.Index(out, "${{")
		if start < 0 {
			return out, nil
		}
		end := strings.Index(out[start:], "}}")
		if end < 0 {
			return "", fmt.Errorf("invalid expression: unterminated ${{ in %q", s)
		}
		inner := strings.TrimSpace(out[start+3 : start+end])
		v, err := f.lookup(inner)
		if err != nil {
			return "", err
		}
		out = out[:start] + v + out[start+end+2:]
	}
}

// lookup walks a dotted path through the contexts map.
func (f *fakeEvaluator) lookup(path string) (string, error) {
	if f.fail[path] {
		return "", fmt.Errorf("unrecognized named-value %q", path)
	}
	var cur any = f.contexts
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", fmt.Errorf("unrecognized named-value %q", path)
		}
		cur, ok = m[part]
		if !ok {
			return "", nil
		}
	}
	if cur == nil {
		return "", nil
	}
	return fmt.Sprint(cur), nil
}

// fakeResolver serves actions from a temp directory on the host.
type fakeResolver struct {
	dirs  map[string]string // uses -> host dir
	metas map[string]*actions.Metadata
	err   error
}

func (r *fakeResolver) Resolve(_ context.Context, uses string) (actions.Resolved, error) {
	if r.err != nil {
		return actions.Resolved{}, r.err
	}
	ref, err := actions.ParseReference(uses)
	if err != nil {
		return actions.Resolved{}, err
	}
	if ref.Kind != actions.KindRepo {
		return actions.Resolved{Ref: ref}, nil
	}
	dir, ok := r.dirs[uses]
	if !ok {
		return actions.Resolved{}, fmt.Errorf("unable to resolve action %q: reference %q does not exist", uses, ref.Ref)
	}
	meta, err := actions.LoadMetadataDir(dir)
	if err != nil {
		return actions.Resolved{}, err
	}
	if r.metas != nil {
		r.metas[uses] = meta
	}
	return actions.Resolved{Ref: ref, Dir: dir, SHA: "sha-" + ref.Repo, Meta: meta}, nil
}
