package exec

import "embed"

//go:embed testdata/sources
var fxFS embed.FS

// fx reads a fixture body from testdata/sources, so no action metadata lives in a Go string.
func fx(name string) string {
	b, err := fxFS.ReadFile("testdata/sources/" + name)
	if err != nil {
		panic(err)
	}
	return string(b)
}
