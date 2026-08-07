// Package migrations holds the forward-only SQL schema files as an embedded FS.
//
// The runner lives in internal/store/pg; go:embed cannot reach outside a
// package directory, so the files and their FS live together here.
package migrations

import "embed"

// FS holds every NNNN_name.sql file, applied in filename order.
//
//go:embed *.sql
var FS embed.FS
