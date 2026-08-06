// Package web holds the committed esbuild output and embeds it.
//
// The embed directive has to live in this directory: go:embed patterns cannot
// escape their own package with "..", so internal/webui cannot reach a
// top-level web/ on its own. It imports this package's FS instead.
//
// Regenerate with: go run ./cmd/buildweb
package web

import "embed"

//go:embed index.html app.mjs app.css
var files embed.FS

// FS is the built UI: index.html plus the bundled module and stylesheet.
var FS = files
