package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
)

// command is one subcommand. Each lives in its own file and registers itself
// from an init(); main never enumerates them.
type command struct {
	name  string
	short string
	// setup declares flags on fs and returns the function that runs them.
	run func(ctx context.Context, fs *flag.FlagSet, args []string) error
}

var registry = map[string]*command{}

func register(c *command) {
	if _, dup := registry[c.name]; dup {
		panic("duplicate command " + c.name)
	}
	registry[c.name] = c
}

func usage() {
	fmt.Fprintf(os.Stderr, "usage: ci-runner <command> [flags]\n\ncommands:\n")
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(os.Stderr, "  %-10s %s\n", n, registry[n].short)
	}
	fmt.Fprintf(os.Stderr, "\nrun `ci-runner <command> -h` for a command's flags.\n")
}

// envOr reads an environment variable, falling back to a default. It exists so
// every setting has both a flag and an env var without repeating the pattern.
func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}
