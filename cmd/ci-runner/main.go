// Command ci-runner is the runner agent: it claims jobs from the control
// plane and executes each one in a fresh Docker-in-Docker sandbox.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// version is stamped at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	name := os.Args[1]
	cmd, ok := registry[name]
	if !ok {
		fmt.Fprintf(os.Stderr, "ci-runner: unknown command %q\n\n", name)
		usage()
		os.Exit(2)
	}

	// SIGTERM is the graceful shutdown: cancelling the context makes the agent
	// release whatever it is running back to the queue rather than losing it.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fs := flag.NewFlagSet(name, flag.ExitOnError)
	if err := cmd.run(ctx, fs, os.Args[2:]); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		fmt.Fprintf(os.Stderr, "ci-runner %s: %v\n", name, err)
		os.Exit(1)
	}
}
