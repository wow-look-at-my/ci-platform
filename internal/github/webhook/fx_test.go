package webhook

import "embed"

//go:embed testdata/sources
var sources embed.FS

// fx returns the recorded source called name.
func fx(name string) string {
	b, err := sources.ReadFile("testdata/sources/" + name)
	if err != nil {
		panic("ci-platform: missing recorded source " + name + ": " + err.Error())
	}
	return string(b)
}
