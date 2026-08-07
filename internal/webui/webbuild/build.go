// Package webbuild bundles web-src/ into the web/ output with esbuild.
//
// It is a separate package from webui so the server binary links the embedded
// bundle without also linking the bundler.
package webbuild

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
)

// BuildOptions controls one bundle.
type BuildOptions struct {
	SrcDir string
	OutDir string
	// Dev keeps the output readable and emits sourcemaps.
	Dev bool
}

// Files maps an output file name to its bytes.
type Files map[string][]byte

// Build produces the whole web/ payload in memory. Nothing is written; the
// caller writes it or compares it against what is committed.
func Build(o BuildOptions) (Files, error) {
	if o.SrcDir == "" {
		o.SrcDir = "web-src"
	}
	if o.OutDir == "" {
		// esbuild needs an outdir to derive output paths even with Write off.
		o.OutDir = "web"
	}
	entry := filepath.Join(o.SrcDir, "app.ts")
	if _, err := os.Stat(entry); err != nil {
		return nil, fmt.Errorf("entry point %s: %w", entry, err)
	}

	result := api.Build(api.BuildOptions{
		EntryPoints:       []string{entry, filepath.Join(o.SrcDir, "app.css")},
		Bundle:            true,
		Format:            api.FormatESModule,
		Target:            api.ES2022,
		Platform:          api.PlatformBrowser,
		MinifyWhitespace:  !o.Dev,
		MinifyIdentifiers: !o.Dev,
		MinifySyntax:      !o.Dev,
		Sourcemap:         sourcemapMode(o.Dev),
		OutExtension:      map[string]string{".js": ".mjs"},
		Outdir:            o.OutDir,
		Write:             false,
		LogLevel:          api.LogLevelSilent,
		Charset:           api.CharsetUTF8,
	})
	if len(result.Errors) > 0 {
		return nil, fmt.Errorf("bundling failed:\n%s", strings.Join(messages(result.Errors), "\n"))
	}

	files := Files{}
	for _, f := range result.OutputFiles {
		files[filepath.Base(f.Path)] = f.Contents
	}
	if _, ok := files["app.mjs"]; !ok {
		return nil, fmt.Errorf("bundling produced no app.mjs (got %v)", names(files))
	}
	if _, ok := files["app.css"]; !ok {
		return nil, fmt.Errorf("bundling produced no app.css (got %v)", names(files))
	}

	html, err := os.ReadFile(filepath.Join(o.SrcDir, "index.html"))
	if err != nil {
		return nil, fmt.Errorf("read index.html: %w", err)
	}
	files["index.html"] = stampAssets(html, files)
	return files, nil
}

func sourcemapMode(dev bool) api.SourceMap {
	if dev {
		return api.SourceMapLinked
	}
	return api.SourceMapNone
}

// stampAssets appends a content hash to each asset URL in the HTML. The hashed
// URL is what makes an immutable cache header safe: a rebuilt bundle is a
// different URL, so a stale copy can never be served for the new build.
func stampAssets(html []byte, files Files) []byte {
	out := string(html)
	for _, name := range []string{"app.mjs", "app.css"} {
		body, ok := files[name]
		if !ok {
			continue
		}
		out = strings.ReplaceAll(out, "/"+name, "/"+name+"?v="+ContentHash(body))
	}
	return []byte(out)
}

// ContentHash is the short digest used in asset URLs.
func ContentHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])[:12]
}

// WriteFiles writes the bundle to disk, creating the directory if needed.
func WriteFiles(outDir string, files Files) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", outDir, err)
	}
	for _, name := range names(files) {
		path := filepath.Join(outDir, name)
		if err := os.WriteFile(path, files[name], 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	return nil
}

func names(files Files) []string {
	out := make([]string, 0, len(files))
	for n := range files {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func messages(msgs []api.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		if m.Location != nil {
			out = append(out, fmt.Sprintf("  %s:%d:%d: %s", m.Location.File, m.Location.Line, m.Location.Column, m.Text))
			continue
		}
		out = append(out, "  "+m.Text)
	}
	return out
}
