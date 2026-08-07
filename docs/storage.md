# Storage

One SQLite file holds every run, job, step, queue entry, lease, artifact record,
cache record and event. Logs, artifact bytes and cache archives go to the blob
store (`internal/blob/disk` or `internal/blob/s3`), never into the database.

There is no database to provision, no connection string to get wrong, and no
second container to keep alive. `CIPLATFORM_DATABASE_URL` is a file path.

## Why this is enough

The control plane is one process serving one organisation's CI. The write rate
is bounded by how fast jobs change state — a few writes per job per second at
peak — which is orders of magnitude inside what SQLite does on a local disk.
What it buys, beyond one less moving part: the tests run the production store
everywhere, with no service container and nothing to skip, which is why the
conformance suite can no longer pass by not running.

The limit worth knowing: this is a single-node store. Two control planes cannot
share one file over a network filesystem, and scaling out means moving to a
networked database, not mounting this one twice.

## The choices that are load-bearing

- **`modernc.org/sqlite`, not `mattn/go-sqlite3`.** Pure Go, so `CGO_ENABLED=0`
  stays on, the binaries stay static, and the cross-compile matrix keeps working.
- **One connection** (`SetMaxOpenConns(1)`). SQLite takes a database-wide write
  lock, so concurrent writers would collide and retry; serialising them costs
  nothing here and makes the queue's claim-then-lease sequence atomic without
  row locks. It is also what makes `Dequeue` safe: `FOR UPDATE SKIP LOCKED` has
  no SQLite equivalent, and the single writer is what replaces it.
- **WAL, `busy_timeout`, `foreign_keys=ON`, `synchronous=NORMAL`**, set in the
  DSN so every connection gets them. Foreign keys are off by default in SQLite,
  and the schema's `ON DELETE CASCADE` is load-bearing.
- **Timestamps are fixed-width UTC text** (`2006-01-02T15:04:05.000000000Z`).
  Fixed width means string comparison is chronological comparison, which is what
  `not_before <= ?` and the lease reaper depend on. A value that does not parse
  is an error, never a silent zero time.
- **JSON columns are TEXT with `json_type` CHECKs.** A nil Go map or slice would
  encode as JSON `null` and the CHECK would reject it, so the encoder normalises
  nil to `{}` or `[]`.
- **Label matching walks `json_each`.** Postgres did containment with a GIN
  index; here the candidate rows are already narrowed by the dequeue index
  before any label is read.

## Schema changes

`internal/store/sqlite/schema.sql` is the whole schema, and there is no
migration chain. Nothing is deployed, so a numbered forward-only sequence would
be machinery for a situation that does not exist — and it would turn the schema
into something you reconstruct by replaying files instead of something you read.

`Migrate` applies the file to an empty database and records its sha256. On a
database that already has one, it compares: same fingerprint is a no-op,
different is a hard error naming the divergence. It does not attempt a repair,
because guessing at the difference is how a schema and its code drift apart
silently.

Until the first deployment, changing the schema means editing that file and
starting from a fresh database. The first deployed instance is what earns a
migration chain; adding one before then is cost with no reader.
