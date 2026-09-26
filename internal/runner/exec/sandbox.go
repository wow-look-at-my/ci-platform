package exec

import (
	"context"
	"io"
	"io/fs"
)

// RunRequest is one command to run inside the sandbox.
type RunRequest struct {
	Argv []string
	// Env is the complete environment; the sandbox adds nothing, so no control-plane credential leaks in.
	Env        map[string]string
	WorkingDir string
	Stdout     io.Writer
	Stderr     io.Writer
}

// RunResult is the outcome of a command that actually ran. A non-zero exit is
// not an error: err is reserved for the sandbox itself failing.
type RunResult struct {
	ExitCode int
}

// Sandbox is the isolated environment a job's steps run in. The executor is
// written against this interface so it is testable without docker;
// internal/runner/sandbox implements it with a Docker-in-Docker container.
type Sandbox interface {
	Run(ctx context.Context, req RunRequest) (RunResult, error)
	WriteFile(ctx context.Context, path string, data []byte, mode fs.FileMode) error
	ReadFile(ctx context.Context, path string) ([]byte, error)
	MkdirAll(ctx context.Context, path string) error
	RemoveAll(ctx context.Context, path string) error
	// CopyInto places a host directory's contents at containerPath.
	CopyInto(ctx context.Context, hostDir, containerPath string) error
	// LookPath resolves a binary on the sandbox's PATH; a miss is an error, never an empty "carry on".
	LookPath(ctx context.Context, bin string) (string, error)
}
