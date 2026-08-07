package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Docker runs the docker CLI.
//
// We shell out to the `docker` binary rather than importing the docker SDK:
// the SDK is not among this module's permitted dependencies, and the CLI is
// the same interface an operator debugging a stuck job would type by hand.
type Docker interface {
	Run(ctx context.Context, in Invocation) (int, error)
}

// Invocation is one docker CLI call.
type Invocation struct {
	Args   []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// CLI is the real Docker, talking to the outer daemon.
type CLI struct {
	// Binary is the docker executable, "docker" when empty.
	Binary string
	// Host is the outer DOCKER_HOST; empty uses the daemon's default socket.
	Host string
	// Env is extra environment for the CLI process.
	Env []string
}

// Run executes the docker CLI and returns its exit code. err is non-nil only
// when the process could not be run or was interrupted; a non-zero exit code
// is returned as a value so callers can report the command's own failure.
func (c *CLI) Run(ctx context.Context, in Invocation) (int, error) {
	bin := c.Binary
	if bin == "" {
		bin = "docker"
	}
	cmd := exec.CommandContext(ctx, bin, in.Args...)
	cmd.Stdin = in.Stdin
	cmd.Stdout = in.Stdout
	cmd.Stderr = in.Stderr
	cmd.Env = append(cmd.Environ(), c.Env...)
	if c.Host != "" {
		cmd.Env = append(cmd.Env, "DOCKER_HOST="+c.Host)
	}
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	return -1, fmt.Errorf("running %s %s: %w", bin, strings.Join(in.Args, " "), err)
}

// capture runs a docker command collecting its output, and turns a non-zero
// exit into an error carrying that output: a docker command that failed with a
// message nobody reads is how a sandbox failure becomes a mystery.
func capture(ctx context.Context, d Docker, args ...string) (string, error) {
	var out, errBuf bytes.Buffer
	code, err := d.Run(ctx, Invocation{Args: args, Stdout: &out, Stderr: &errBuf})
	if err != nil {
		return out.String(), err
	}
	if code != 0 {
		msg := strings.TrimSpace(errBuf.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		return out.String(), fmt.Errorf("docker %s exited %d: %s", strings.Join(args, " "), code, msg)
	}
	return out.String(), nil
}
