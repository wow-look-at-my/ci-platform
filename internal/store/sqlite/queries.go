package sqlite

import _ "embed"

//go:embed testdata/sources/artifact_usage.sql
var artifactUsageQuery string
