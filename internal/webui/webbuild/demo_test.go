package webbuild_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wow-look-at-my/ci-platform/internal/demoseed"
	"github.com/wow-look-at-my/ci-platform/internal/webui/webbuild"
)

func srcDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "..", "web-src"))
	require.NoError(t, err)
	return dir
}

// The demo build must be the real UI with only its data swapped. If it started
// bundling its own copy of a page, the demo would stop being evidence about the
// product.
func TestDemoBuildSwapsTheClientAndNothingElse(t *testing.T) {
	files, err := webbuild.Build(webbuild.BuildOptions{SrcDir: srcDir(t), Demo: true})
	require.NoError(t, err)

	js := string(files["app.mjs"])
	assert.Contains(t, js, "captured response for",
		"the demo client's own message should be in the bundle")
	assert.NotContains(t, js, `"/api/v1/runs${`,
		"the live client's URL building must not be bundled into the demo")
	assert.NotContains(t, js, "/auth/login",
		"the demo has no session, so the sign-in request must not be bundled")

	html := string(files["index.html"])
	assert.Contains(t, html, "captured snapshot", "the demo must say what it is")
}

// The production build must not pick the demo up: a shipped bundle carrying
// fixture data would serve fake runs to a real operator.
func TestProductionBuildHasNoDemoInIt(t *testing.T) {
	files, err := webbuild.Build(webbuild.BuildOptions{SrcDir: srcDir(t)})
	require.NoError(t, err)

	js := string(files["app.mjs"])
	assert.NotContains(t, js, "captured response for")
	assert.NotContains(t, js, "acme/widget", "no fixture data may reach the shipped bundle")
	assert.Contains(t, string(files["index.html"]), "/app.mjs")
	assert.NotContains(t, string(files["index.html"]), "captured snapshot")
}

// The banner states when the snapshot was taken, because every relative time on
// the page is relative to that instant. A hard-coded date that drifts from the
// seed's clock would make the whole page quietly wrong.
func TestDemoBannerNamesTheCaptureTime(t *testing.T) {
	html, err := os.ReadFile(filepath.Join(srcDir(t), "demo", "index.html"))
	require.NoError(t, err)

	want := demoseed.Now.UTC().Format("2006-01-02 15:04") + " UTC"
	assert.Contains(t, string(html), want,
		"the banner must name demoseed.Now; update web-src/demo/index.html when the seed's clock moves")
}

// A demo built for a site branch is served under a path prefix, so an absolute
// asset URL would 404 there.
func TestDemoAssetsAreRelative(t *testing.T) {
	files, err := webbuild.Build(webbuild.BuildOptions{SrcDir: srcDir(t), Demo: true})
	require.NoError(t, err)

	html := string(files["index.html"])
	for _, asset := range []string{"app.mjs", "app.css"} {
		assert.Contains(t, html, "./"+asset)
		assert.NotContains(t, html, `"/`+asset, "an absolute asset URL breaks under a site path prefix")
	}
	assert.True(t, strings.Contains(html, "?v="), "assets stay content-stamped in the demo too")
}
