package sqlite

import "embed"

//go:embed testdata/sources/artifact_usage.sql
var artifactUsageQuery string
