// Command buildweb bundles web-src/ into web/ with esbuild. No npm, no Node:
// the bundler is a Go library, so CI and the image need neither.
//
// The built output is committed so go:embed works without a build step, which
// makes it possible for the committed bundle to drift from the sources. -check
// re-bundles into a temp dir and fails on any difference.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wow-look-at-my/ci-platform/internal/webui/webbuild"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "buildweb: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("buildweb", flag.ContinueOnError)
	fs.SetOutput(out)
	var (
		srcDir  = fs.String("src", "web-src", "TypeScript and CSS sources")
		outDir  = fs.String("out", "web", "bundle output directory")
		dev     = fs.Bool("dev", false, "unminified, with sourcemaps")
		check   = fs.Bool("check", false, "re-bundle and fail if the committed output differs")
		verbose = fs.Bool("v", false, "list every emitted file")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	opts := webbuild.BuildOptions{SrcDir: *srcDir, OutDir: *outDir, Dev: *dev}
	if *check {
		if err := runCheck(opts); err != nil {
			return err
		}
		fmt.Fprintln(out, "buildweb -check: committed bundle matches a fresh build")
		return nil
	}

	files, err := webbuild.Build(opts)
	if err != nil {
		return err
	}
	if err := webbuild.WriteFiles(*outDir, files); err != nil {
		return err
	}
	if *verbose {
		names := make([]string, 0, len(files))
		for name, body := range files {
			names = append(names, fmt.Sprintf("%s (%d bytes)", name, len(body)))
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Fprintln(out, "  "+n)
		}
	}
	fmt.Fprintf(out, "buildweb: wrote %d files to %s\n", len(files), *outDir)
	return nil
}

// buildOptions is the default source and output layout, rooted at root.
func buildOptions(root string) webbuild.BuildOptions {
	return webbuild.BuildOptions{SrcDir: filepath.Join(root, "web-src"), OutDir: filepath.Join(root, "web")}
}

// runCheck compares a fresh build against what is on disk.
func runCheck(opts webbuild.BuildOptions) error {
	fresh, err := webbuild.Build(opts)
	if err != nil {
		return err
	}
	var problems []string
	for name, want := range fresh {
		got, err := os.ReadFile(filepath.Join(opts.OutDir, name))
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s is missing from the committed bundle (%v)", name, err))
			continue
		}
		if string(got) != string(want) {
			problems = append(problems, fmt.Sprintf("%s differs from a fresh build (committed %d bytes, fresh %d bytes)", name, len(got), len(want)))
		}
	}
	entries, err := os.ReadDir(opts.OutDir)
	if err != nil {
		return fmt.Errorf("read %s: %w", opts.OutDir, err)
	}
	for _, e := range entries {
		// embed.go lives beside the bundle so go:embed can reach it; it is
		// source, not build output.
		if e.IsDir() || strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		if _, ok := fresh[e.Name()]; !ok {
			problems = append(problems, fmt.Sprintf("%s is committed but no longer produced by a build", e.Name()))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("the committed web/ bundle is stale:\n  %s\nrun: go run ./cmd/buildweb", strings.Join(problems, "\n  "))
	}
	return nil
}
