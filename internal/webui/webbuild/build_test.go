package webbuild

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fixtureSrc(t *testing.T, entry string) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644))
	}
	write("app.ts", entry)
	write("app.css", "body { color: red; }\n")
	write("index.html", `<link rel="stylesheet" href="/app.css"><script src="/app.mjs"></script>`)
	return dir
}

func TestBuildEmitsTheThreeAssets(t *testing.T) {
	src := fixtureSrc(t, "export const answer: number = 42;\nconsole.log(answer);\n")
	files, err := Build(BuildOptions{SrcDir: src, OutDir: filepath.Join(t.TempDir(), "web")})
	require.NoError(t, err)

	require.Contains(t, files, "app.mjs")
	require.Contains(t, files, "app.css")
	require.Contains(t, files, "index.html")
	assert.Contains(t, string(files["app.css"]), "color:")
	assert.NotContains(t, string(files["app.mjs"]), ": number", "types must be stripped")
}

func TestBuildStampsAssetURLsWithTheirContentHash(t *testing.T) {
	src := fixtureSrc(t, "console.log(1);\n")
	files, err := Build(BuildOptions{SrcDir: src})
	require.NoError(t, err)

	html := string(files["index.html"])
	assert.Contains(t, html, "/app.mjs?v="+ContentHash(files["app.mjs"]))
	assert.Contains(t, html, "/app.css?v="+ContentHash(files["app.css"]))
}

func TestBuildIsDeterministic(t *testing.T) {
	src := fixtureSrc(t, "export const x = { a: 1, b: 2 };\nconsole.log(x);\n")
	a, err := Build(BuildOptions{SrcDir: src})
	require.NoError(t, err)
	b, err := Build(BuildOptions{SrcDir: src})
	require.NoError(t, err)
	for name := range a {
		assert.Equal(t, string(a[name]), string(b[name]), "%s differed between two builds", name)
	}
}

func TestDevBuildKeepsSourcesReadableAndEmitsAMap(t *testing.T) {
	src := fixtureSrc(t, "const aVeryDescriptiveName = 1;\nconsole.log(aVeryDescriptiveName);\n")
	files, err := Build(BuildOptions{SrcDir: src, Dev: true})
	require.NoError(t, err)
	assert.Contains(t, string(files["app.mjs"]), "aVeryDescriptiveName")
	assert.Contains(t, files, "app.mjs.map")
}

func TestBuildReportsCompileErrorsWithTheirLocation(t *testing.T) {
	src := fixtureSrc(t, "import { nope } from \"./does-not-exist.js\";\nconsole.log(nope);\n")
	_, err := Build(BuildOptions{SrcDir: src})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bundling failed")
	assert.Contains(t, err.Error(), "does-not-exist")
}

func TestBuildRefusesAMissingEntryPoint(t *testing.T) {
	_, err := Build(BuildOptions{SrcDir: filepath.Join(t.TempDir(), "nothing-here")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "entry point")
}

func TestBuildRefusesAMissingIndexHTML(t *testing.T) {
	src := fixtureSrc(t, "console.log(1);\n")
	require.NoError(t, os.Remove(filepath.Join(src, "index.html")))
	_, err := Build(BuildOptions{SrcDir: src})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "index.html")
}

func TestWriteFilesCreatesTheDirectoryAndTheContents(t *testing.T) {
	out := filepath.Join(t.TempDir(), "nested", "web")
	require.NoError(t, WriteFiles(out, Files{"a.txt": []byte("hello"), "b.txt": []byte("world")}))
	got, err := os.ReadFile(filepath.Join(out, "a.txt"))
	require.NoError(t, err)
	assert.Equal(t, "hello", string(got))
}

func TestContentHashIsShortStableAndInputSensitive(t *testing.T) {
	a := ContentHash([]byte("one"))
	assert.Len(t, a, 12)
	assert.Equal(t, a, ContentHash([]byte("one")))
	assert.NotEqual(t, a, ContentHash([]byte("two")))
	assert.False(t, strings.ContainsAny(a, "/?&"), "the hash goes in a URL")
}

func TestDefaultSrcDirIsWebSrc(t *testing.T) {
	// Run from a directory with no web-src so the default is exercised and the
	// error names it.
	wd, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(t.TempDir()))
	t.Cleanup(func() { _ = os.Chdir(wd) })

	_, err = Build(BuildOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), filepath.Join("web-src", "app.ts"))
}
