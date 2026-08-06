// Package sandbox builds one Docker-in-Docker container per job: a privileged
// container running its own dockerd and its own network, torn down with the
// job, so no filesystem or process a job creates outlives it.
//
// The image store is the deliberate exception. When ImageCacheVolume is set,
// the inner daemons share one graph directory under an exclusive lock, so an
// image a job builds or pulls IS visible to the next job on that runner --
// that is the point of the cache, and it is why the lock exists. A job that
// must not see another job's layers needs its own runner, not its own
// container.
//
// see docs/security.md
package sandbox
