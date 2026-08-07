// Package deps pins the module's third-party dependency set in one place.
//
// It exists so that every package can be developed against an already-resolved
// go.mod: without it, the first package to import a new module rewrites go.sum,
// and parallel work on separate packages fights over the same two files.
package deps

import (
	// Workflow YAML parsing.
	_ "gopkg.in/yaml.v3"

	// SQLite, the production metadata store. The pure-Go driver keeps CGO off.
	_ "modernc.org/sqlite"

	// GitHub App JWTs and the OIDC tokens issued to jobs.
	_ "github.com/golang-jwt/jwt/v5"

	// Runner and job identifiers.
	_ "github.com/google/uuid"

	// Cron parsing for schedule: triggers.
	_ "github.com/robfig/cron/v3"

	// Bundling the TypeScript UI at build time, so no Node toolchain is needed
	// in CI or in the image.
	_ "github.com/evanw/esbuild/pkg/api"

	// Assertions, which go-toolchain's fixer rewrites tests into anyway.
	_ "github.com/stretchr/testify/assert"
	_ "github.com/stretchr/testify/require"
)
