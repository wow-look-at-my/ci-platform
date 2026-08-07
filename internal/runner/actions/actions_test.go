package actions

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseReference(t *testing.T) {
	tests := []struct {
		in    string
		kind  Kind
		owner string
		repo  string
		path  string
		ref   string
		local string
		image string
	}{
		{in: "actions/checkout@v4", kind: KindRepo, owner: "actions", repo: "checkout", ref: "v4"},
		{in: "org/repo/sub/dir@abc123", kind: KindRepo, owner: "org", repo: "repo", path: "sub/dir", ref: "abc123"},
		{in: "./.github/actions/build", kind: KindLocal, local: ".github/actions/build"},
		{in: "docker://alpine:3.20", kind: KindDocker, image: "alpine:3.20"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			ref, err := ParseReference(tt.in)
			require.NoError(t, err)
			assert.Equal(t, tt.kind, ref.Kind)
			assert.Equal(t, tt.owner, ref.Owner)
			assert.Equal(t, tt.repo, ref.Repo)
			assert.Equal(t, tt.path, ref.Path)
			assert.Equal(t, tt.ref, ref.Ref)
			assert.Equal(t, tt.local, ref.LocalPath)
			assert.Equal(t, tt.image, ref.Image)
			assert.Equal(t, tt.in, ref.String())
		})
	}
}

func TestParseReferenceErrors(t *testing.T) {
	for name, in := range map[string]string{
		"empty":          "",
		"no ref":         "actions/checkout",
		"empty ref":      "actions/checkout@",
		"not owner/repo": "checkout@v4",
		"absolute path":  "/etc/passwd",
		"escapes":        "./../../etc",
		"no image":       "docker://",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseReference(in)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "uses:")
		})
	}
}

func TestCacheKey(t *testing.T) {
	ref, err := ParseReference("actions/checkout@v4")
	require.NoError(t, err)
	assert.Equal(t, "actions/checkout@deadbeef", ref.CacheKey("deadbeef"))
}

const jsActionYAML = `
name: Test Action
description: does a thing
inputs:
  who-to-greet:
    description: name
    required: true
    default: World
  token:
    description: a token
    required: true
  old-input:
    description: legacy
    deprecationMessage: use who-to-greet
outputs:
  time:
    description: when
runs:
  using: node20
  main: dist/index.js
  pre: dist/pre.js
  post: dist/post.js
`

func TestParseMetadataJavaScript(t *testing.T) {
	m, err := ParseMetadata([]byte(jsActionYAML))
	require.NoError(t, err)
	assert.Equal(t, "Test Action", m.Name)
	assert.True(t, m.Runs.IsJavaScript())
	assert.Equal(t, "20", m.Runs.NodeVersion())
	assert.False(t, m.Runs.IsComposite())
	assert.Equal(t, "dist/index.js", m.Runs.Main)
	assert.Equal(t, "dist/post.js", m.Runs.Post)
	assert.Equal(t, []string{"who-to-greet", "token", "old-input"}, m.InputOrder)
	assert.True(t, m.Inputs["who-to-greet"].HasDefault)
	assert.Equal(t, "World", m.Inputs["who-to-greet"].Default)
	assert.False(t, m.Inputs["token"].HasDefault)
	assert.Contains(t, m.Outputs, "time")
}

func TestParseMetadataComposite(t *testing.T) {
	m, err := ParseMetadata([]byte(`
name: Composite
description: d
inputs:
  greeting:
    default: hi
outputs:
  result:
    description: r
    value: ${{ steps.one.outputs.v }}
runs:
  using: composite
  steps:
    - id: one
      run: echo ${{ inputs.greeting }}
      shell: bash
    - uses: actions/checkout@v4
      with:
        ref: main
`))
	require.NoError(t, err)
	require.True(t, m.Runs.IsComposite())
	require.Len(t, m.Runs.Steps, 2)
	assert.Equal(t, "one", m.Runs.Steps[0].ID)
	assert.Equal(t, "bash", m.Runs.Steps[0].Shell)
	assert.Equal(t, "actions/checkout@v4", m.Runs.Steps[1].Uses)
	assert.Equal(t, "main", m.Runs.Steps[1].With["ref"])
	assert.Equal(t, "${{ steps.one.outputs.v }}", m.Outputs["result"].Value)
}

func TestParseMetadataDockerAndErrors(t *testing.T) {
	m, err := ParseMetadata([]byte("name: d\nruns:\n  using: docker\n  image: Dockerfile\n"))
	require.NoError(t, err)
	assert.True(t, m.Runs.IsDocker())

	_, err = ParseMetadata([]byte("name: x\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "runs.using is missing")

	_, err = ParseMetadata([]byte("\tnot yaml: ["))
	require.Error(t, err)

	_, err = ParseMetadata([]byte("runs:\n  using: node20\n  main: i.js\ninputs: notamap\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "inputs must be a mapping")

	_, err = ParseMetadata([]byte("runs:\n  using: node20\n  main: i.js\noutputs: notamap\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "outputs must be a mapping")
}

func TestInputEnv(t *testing.T) {
	m, err := ParseMetadata([]byte(jsActionYAML))
	require.NoError(t, err)

	env, warnings, err := m.InputEnv(map[string]string{"token": "t", "old-input": "x", "extra thing": "e"})
	require.NoError(t, err)
	assert.Equal(t, "World", env["INPUT_WHO-TO-GREET"], "declared default applies")
	assert.Equal(t, "t", env["INPUT_TOKEN"])
	assert.Equal(t, "e", env["INPUT_EXTRA_THING"], "spaces become underscores")
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "deprecated")
}

func TestInputEnvRequiredWithoutValueIsConfigError(t *testing.T) {
	m, err := ParseMetadata([]byte(jsActionYAML))
	require.NoError(t, err)
	_, _, err = m.InputEnv(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported:")
	assert.Contains(t, err.Error(), `"token"`)
}

func TestInputValues(t *testing.T) {
	m, err := ParseMetadata([]byte(jsActionYAML))
	require.NoError(t, err)
	vals, err := m.InputValues(map[string]string{"token": "t", "custom": "c"})
	require.NoError(t, err)
	assert.Equal(t, "World", vals["who-to-greet"])
	assert.Equal(t, "t", vals["token"])
	assert.Equal(t, "c", vals["custom"])
	assert.Equal(t, "", vals["old-input"], "an undeclared value reads as empty, never missing")

	_, err = m.InputValues(nil)
	require.Error(t, err)
}

func TestInputEnvName(t *testing.T) {
	assert.Equal(t, "INPUT_WHO_TO_GREET", InputEnvName("who to greet"))
	assert.Equal(t, "INPUT_TOKEN", InputEnvName("token"))
}

// fakeFetcher serves an in-memory repository tarball and counts downloads.
type fakeFetcher struct {
	sha       string
	files     map[string]string
	downloads int
	refErr    error
	tarErr    error
}

func (f *fakeFetcher) ResolveRef(context.Context, string, string, string) (string, error) {
	return f.sha, f.refErr
}

func (f *fakeFetcher) Tarball(context.Context, string, string, string) (io.ReadCloser, error) {
	if f.tarErr != nil {
		return nil, f.tarErr
	}
	f.downloads++
	return io.NopCloser(bytes.NewReader(makeTarGz(f.files))), nil
}

func makeTarGz(files map[string]string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	// Code hosts wrap the repository in a single top-level directory.
	_ = tw.WriteHeader(&tar.Header{Name: "repo-sha/", Typeflag: tar.TypeDir, Mode: 0o755})
	for name, body := range files {
		_ = tw.WriteHeader(&tar.Header{
			Name: "repo-sha/" + name, Typeflag: tar.TypeReg,
			Mode: 0o644, Size: int64(len(body)),
		})
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func TestResolverFetchesAndCaches(t *testing.T) {
	dir := t.TempDir()
	f := &fakeFetcher{sha: "abc123", files: map[string]string{"action.yml": jsActionYAML}}
	r := NewResolver(dir, f)

	got, err := r.Resolve(context.Background(), "octo/act@v1")
	require.NoError(t, err)
	assert.Equal(t, "abc123", got.SHA)
	assert.False(t, got.Cached)
	require.NotNil(t, got.Meta)
	assert.Equal(t, "Test Action", got.Meta.Name)
	assert.FileExists(t, filepath.Join(got.Dir, "action.yml"))
	assert.Equal(t, 1, f.downloads)

	// A second resolve of the same ref must not hit the network again.
	r2 := NewResolver(dir, f)
	got2, err := r2.Resolve(context.Background(), "octo/act@v1")
	require.NoError(t, err)
	assert.True(t, got2.Cached)
	assert.Equal(t, 1, f.downloads)
}

func TestResolverSubdirectoryAndYamlExtension(t *testing.T) {
	f := &fakeFetcher{sha: "s1", files: map[string]string{
		"sub/dir/action.yaml": "runs:\n  using: composite\n  steps: []\n",
	}}
	r := NewResolver(t.TempDir(), f)
	got, err := r.Resolve(context.Background(), "o/p/sub/dir@main")
	require.NoError(t, err)
	require.NotNil(t, got.Meta)
	assert.True(t, got.Meta.Runs.IsComposite())
}

func TestResolverLocalAndDockerAreNotFetched(t *testing.T) {
	r := NewResolver(t.TempDir(), &fakeFetcher{})
	local, err := r.Resolve(context.Background(), "./x/y")
	require.NoError(t, err)
	assert.Equal(t, KindLocal, local.Ref.Kind)
	assert.Empty(t, local.Dir)

	d, err := r.Resolve(context.Background(), "docker://alpine")
	require.NoError(t, err)
	assert.Equal(t, KindDocker, d.Ref.Kind)
}

func TestResolverUnresolvableRefNamesTheRef(t *testing.T) {
	r := NewResolver(t.TempDir(), &fakeFetcher{refErr: errors.New("404 Not Found")})
	_, err := r.Resolve(context.Background(), "octo/missing@v9")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unable to resolve action")
	assert.Contains(t, err.Error(), "octo/missing@v9")

	r2 := NewResolver(t.TempDir(), &fakeFetcher{sha: ""})
	_, err = r2.Resolve(context.Background(), "octo/missing@v9")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not exist")
}

func TestResolverWithoutFetcher(t *testing.T) {
	r := NewResolver(t.TempDir(), nil)
	_, err := r.Resolve(context.Background(), "o/p@v1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no action fetcher")
}

func TestResolverMissingActionYAML(t *testing.T) {
	r := NewResolver(t.TempDir(), &fakeFetcher{sha: "s", files: map[string]string{"README.md": "hi"}})
	_, err := r.Resolve(context.Background(), "o/p@v1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no action.yml")
}

func TestExtractRejectsPathEscape(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := "pwned"
	_ = tw.WriteHeader(&tar.Header{Name: "repo/../../escape", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})
	_, _ = tw.Write([]byte(body))
	_ = tw.Close()
	_ = gz.Close()

	err := extractTarGz(bytes.NewReader(buf.Bytes()), t.TempDir())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes")
}

func TestExtractHandlesDirsAndSymlinks(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "repo/sub/", Typeflag: tar.TypeDir, Mode: 0o755})
	_ = tw.WriteHeader(&tar.Header{Name: "repo/sub/link", Typeflag: tar.TypeSymlink, Linkname: "target"})
	_ = tw.Close()
	_ = gz.Close()

	dest := t.TempDir()
	require.NoError(t, extractTarGz(bytes.NewReader(buf.Bytes()), dest))
	fi, err := os.Lstat(filepath.Join(dest, "sub", "link"))
	require.NoError(t, err)
	assert.NotZero(t, fi.Mode()&os.ModeSymlink)
}

func TestExtractRejectsNonGzip(t *testing.T) {
	require.Error(t, extractTarGz(bytes.NewReader([]byte("not a tarball")), t.TempDir()))
}

func TestHTTPFetcher(t *testing.T) {
	tarball := makeTarGz(map[string]string{"action.yml": jsActionYAML})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer tok", r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/repos/o/p/commits/v1":
			_, _ = w.Write([]byte("  sha123\n"))
		case "/repos/o/p/commits/json":
			_, _ = w.Write([]byte(`{"sha":"jsonsha"}`))
		case "/repos/o/p/commits/bad":
			_, _ = w.Write([]byte(`{bad json`))
		case "/repos/o/p/tarball/sha123":
			_, _ = w.Write(tarball)
		default:
			http.Error(w, "nope", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	f := NewHTTPFetcher(srv.URL, "tok")
	sha, err := f.ResolveRef(context.Background(), "o", "p", "v1")
	require.NoError(t, err)
	assert.Equal(t, "sha123", sha)

	sha, err = f.ResolveRef(context.Background(), "o", "p", "json")
	require.NoError(t, err)
	assert.Equal(t, "jsonsha", sha)

	_, err = f.ResolveRef(context.Background(), "o", "p", "bad")
	require.Error(t, err)

	_, err = f.ResolveRef(context.Background(), "o", "p", "missing")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP 404")

	rc, err := f.Tarball(context.Background(), "o", "p", "sha123")
	require.NoError(t, err)
	defer rc.Close()
	dest := t.TempDir()
	require.NoError(t, extractTarGz(rc, dest))
	assert.FileExists(t, filepath.Join(dest, "action.yml"))
}

func TestResolverEndToEndOverHTTP(t *testing.T) {
	tarball := makeTarGz(map[string]string{"action.yml": jsActionYAML})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if filepath.Base(filepath.Dir(r.URL.Path)) == "commits" {
			_, _ = w.Write([]byte("thesha"))
			return
		}
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()

	r := NewResolver(t.TempDir(), NewHTTPFetcher(srv.URL, ""))
	got, err := r.Resolve(context.Background(), "o/p@v2")
	require.NoError(t, err)
	assert.Equal(t, "thesha", got.SHA)
	assert.Equal(t, "Test Action", got.Meta.Name)
}
