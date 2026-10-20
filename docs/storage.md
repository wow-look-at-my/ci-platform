# Storage

One SQLite file holds every run, job, step, queue entry, lease, artifact record, cache record and event. Logs, artifact bytes and cache archives go to the blob store (`internal/blob/disk` or `internal/blob/s3`), not into the database.

There is no database to provision, no connection string to get incorrect, and no second container to keep alive. `CIPLATFORM_DATABASE_URL` is a file path.

## Why this is enough

The control plane is one process serving one organisation's CI. How fast jobs change state bounds the write rate — a few writes per job per second at peak. Which is orders of magnitude inside what SQLite does on a local disk. What it buys, beyond one less moving part: the tests run the production store everywhere, with no service container and nothing to skip. This is why the conformance suite can no longer pass by not running.

The limit worth knowing: this is a single-node store. Control planes cannot share one file over a network filesystem, and scaling out means moving to a networked database, not mounting this twice.

## The choices that are load-bearing

- **`modernc.org/sqlite`, not `mattn/go-sqlite3`.** Pure Go, so `CGO_ENABLED=0` stays on, the binaries stay static, and the cross-compile matrix keeps working.
- **One connection** (`SetMaxOpenConns(1)`). SQLite takes a database-wide write lock, so concurrent writers will collide and retry. Serialising them costs nothing here and makes the queue's claim-then-lease sequence atomic without row locks. It is also what makes `Dequeue` safe: `FOR UPDATE SKIP LOCKED` has no SQLite equivalent. The single writer is what replaces it.
- **WAL, `busy_timeout`, `foreign_keys=ON`, `synchronous=NORMAL`**, set in the DSN so every connection gets them. Foreign keys are off by default in SQLite, and the schema's `ON DELETE CASCADE` is load-bearing.
- **Timestamps are fixed-width UTC text** (`2006-01-02T15:04:05.000000000Z`). Fixed width means string comparison is chronological comparison, which is what `not_before <= ?` and the lease reaper depend on. A value that does not parse is an error, not a silent zero time.
- **JSON columns are TEXT with `json_type` CHECKs.** A nil Go map or slice will encode as JSON `null`. The CHECK will reject it, so the encoder normalises nil to `{}` or `[]`.
- **Label matching walks `json_each`.** Postgres did containment with a GIN index. Here the candidate rows are already narrowed by the dequeue index before any label is read.

## Schema changes

`internal/store/sqlite/schema.sql` is the whole schema, and there is no migration chain. Nothing is deployed, so a numbered forward-only sequence will be machinery for a situation that does not exist. It will turn the schema into something you reconstruct by replaying files instead of something you read.

`Migrate` applies the file to an empty database and records its sha256. On a database that already has one, it compares: same fingerprint is a no-op, different is a hard error naming the divergence. It does not try a repair, because guessing at the difference is how a schema and its code drift apart silently.

Until the first deployment, changing the schema means editing that file and starting from a fresh database. The first deployed instance is what earns a migration chain. Adding one before then is cost with no reader.
