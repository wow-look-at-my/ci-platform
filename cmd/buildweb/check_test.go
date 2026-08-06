package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wow-look-at-my/ci-platform/internal/webui/webbuild"
)

// stagedBundle builds the real sources into a scratch directory, so the check
// can then be shown failing on a deliberately corrupted copy.
func stagedBundle(t *testing.T) webbuild.BuildOptions {
	t.Helper()
	opts := buildOptions(repoRoot(t))
	opts.OutDir = filepath.Join(t.TempDir(), "web")
	files, err := webbuild.Build(opts)
	require.NoError(t, err)
	require.NoError(t, webbuild.WriteFiles(opts.OutDir, files))
	return opts
}

func TestCheckPassesOnAFreshlyWrittenBundle(t *testing.T) {
	require.NoError(t, runCheck(stagedBundle(t)))
}

func TestCheckFailsOnModifiedOutput(t *testing.T) {
	opts := stagedBundle(t)
	require.NoError(t, os.WriteFile(filepath.Join(opts.OutDir, "app.mjs"), []byte("// hand-edited\n"), 0o644))
	err := runCheck(opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "app.mjs differs")
	assert.Contains(t, err.Error(), "go run ./cmd/buildweb")
}

func TestCheckFailsOnMissingOutput(t *testing.T) {
	opts := stagedBundle(t)
	require.NoError(t, os.Remove(filepath.Join(opts.OutDir, "app.css")))
	err := runCheck(opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "app.css is missing")
}

func TestCheckFailsOnAnOrphanedFile(t *testing.T) {
	opts := stagedBundle(t)
	require.NoError(t, os.WriteFile(filepath.Join(opts.OutDir, "leftover.mjs"), []byte("x"), 0o644))
	err := runCheck(opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "leftover.mjs is committed but no longer produced")
}

func TestCheckIgnoresTheEmbedSource(t *testing.T) {
	opts := stagedBundle(t)
	require.NoError(t, os.WriteFile(filepath.Join(opts.OutDir, "embed.go"), []byte("package web\n"), 0o644))
	require.NoError(t, runCheck(opts), "embed.go is source that lives beside the bundle, not build output")
}

func TestCheckReportsABrokenBuildRatherThanPassing(t *testing.T) {
	opts := buildOptions(repoRoot(t))
	opts.SrcDir = filepath.Join(t.TempDir(), "no-sources-here")
	err := runCheck(opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "entry point")
}

func TestRunBuildsIntoTheGivenDirectory(t *testing.T) {
	root := repoRoot(t)
	out := filepath.Join(t.TempDir(), "web")
	var buf bytes.Buffer
	require.NoError(t, run([]string{"-src", filepath.Join(root, "web-src"), "-out", out, "-v"}, &buf))
	assert.Contains(t, buf.String(), "app.mjs")
	assert.Contains(t, buf.String(), "wrote 3 files")
	for _, name := range []string{"app.mjs", "app.css", "index.html"} {
		_, err := os.Stat(filepath.Join(out, name))
		require.NoError(t, err, name)
	}
}

func TestRunCheckModeReportsFreshAndStale(t *testing.T) {
	root := repoRoot(t)
	var buf bytes.Buffer
	require.NoError(t, run([]string{"-src", filepath.Join(root, "web-src"), "-out", filepath.Join(root, "web"), "-check"}, &buf))
	assert.Contains(t, buf.String(), "matches a fresh build")

	opts := stagedBundle(t)
	require.NoError(t, os.Remove(filepath.Join(opts.OutDir, "app.mjs")))
	err := run([]string{"-src", opts.SrcDir, "-out", opts.OutDir, "-check"}, &buf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "app.mjs is missing")
}

func TestRunSurfacesABadFlag(t *testing.T) {
	require.Error(t, run([]string{"-nonsense"}, io.Discard))
}
