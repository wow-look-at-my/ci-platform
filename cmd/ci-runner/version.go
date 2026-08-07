package main

import (
	"context"
	"flag"
	"fmt"
	"runtime"
)

func init() {
	register(&command{
		name:  "version",
		short: "print the runner version and platform",
		run: func(_ context.Context, fs *flag.FlagSet, args []string) error {
			if err := fs.Parse(args); err != nil {
				return err
			}
			fmt.Printf("ci-runner %s %s/%s (go %s)\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version())
			return nil
		},
	})
}
