package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evanw/esbuild/pkg/api"
	"github.com/stretchr/testify/require"
)

// nodeCandidates are where a Node binary is expected. The TypeScript tests are
// bundled to plain JS first, so the runtime needs no TS support of its own.
var nodeCandidates = []string{"/opt/node22/bin/node", "node"}

func findNode() string {
	for _, c := range nodeCandidates {
		if p, err := exec.LookPath(c); err == nil {
			return p
		}
	}
	return ""
}

// TestTypeScriptUnits bundles every web-src/*.test.ts and runs them under Node.
// The pure UI logic -- ANSI parsing, group folding, DAG layout, duration
// formatting, failure-first focus -- is tested here rather than in Go.
func TestTypeScriptUnits(t *testing.T) {
	node := findNode()
	if node == "" {
		t.Skipf("no node binary found (looked for %s); the TypeScript unit tests need one to run", strings.Join(nodeCandidates, ", "))
	}

	root := repoRoot(t)
	srcDir := filepath.Join(root, "web-src")
	tests, err := filepath.Glob(filepath.Join(srcDir, "*.test.ts"))
	require.NoError(t, err)
	require.NotEmpty(t, tests, "no *.test.ts files found in %s", srcDir)

	var imports strings.Builder
	for _, f := range tests {
		imports.WriteString("import \"./" + filepath.Base(f) + "\";\n")
	}
	imports.WriteString("import { runAll } from \"./testing.js\";\nprocess.exit(runAll());\n")

	result := api.Build(api.BuildOptions{
		Stdin: &api.StdinOptions{
			Contents:   imports.String(),
			ResolveDir: srcDir,
			Sourcefile: "testmain.ts",
			Loader:     api.LoaderTS,
		},
		Bundle:   true,
		Format:   api.FormatESModule,
		Platform: api.PlatformNode,
		Target:   api.ES2022,
		Write:    false,
		LogLevel: api.LogLevelSilent,
	})
	require.Empty(t, result.Errors, "bundling the TypeScript tests failed: %v", result.Errors)
	require.Len(t, result.OutputFiles, 1)

	bundle := filepath.Join(t.TempDir(), "tests.mjs")
	require.NoError(t, os.WriteFile(bundle, result.OutputFiles[0].Contents, 0o644))

	cmd := exec.Command(node, bundle)
	out, err := cmd.CombinedOutput()
	t.Log("\n" + string(out))
	require.NoError(t, err, "TypeScript unit tests failed")
}

// TestCommittedBundleIsFresh is the same gate as `go run ./cmd/buildweb -check`,
// so a stale committed bundle fails the Go test run too.
func TestCommittedBundleIsFresh(t *testing.T) {
	root := repoRoot(t)
	require.NoError(t, runCheck(buildOptions(root)))
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	require.NoError(t, err)
	// The test runs in cmd/buildweb.
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}
