-- Initial schema: repos, runs, jobs, steps, the durable queue and its lease
-- protocol, runners, annotations, artifacts, caches, secrets, vars, events.
--
-- Conventions:
--   * every timestamp is TEXT, written as a fixed-width UTC RFC 3339 string
--     ('2006-01-02T15:04:05.000000000Z'), so lexicographic comparison is
--     chronological comparison and no local time can ever be stored
--   * every enum-ish column is TEXT with a CHECK listing the valid values, so a
--     typo fails at write time instead of reading back as something else
--   * every structured column is TEXT holding JSON, with a json_type CHECK
--     where the shape (array vs object) is load-bearing
--   * every boolean is INTEGER 0/1, CHECKed, because SQLite has no bool

CREATE TABLE repos (
    id              INTEGER PRIMARY KEY,
    owner           TEXT NOT NULL,
    name            TEXT NOT NULL,
    installation_id INTEGER NOT NULL DEFAULT 0,
    default_branch  TEXT NOT NULL DEFAULT '',
    private         INTEGER NOT NULL DEFAULT 0 CHECK (private IN (0, 1)),
    UNIQUE (owner, name)
);

-- run_numbers is the per-(repo, workflow) monotonic counter behind
-- NextRunNumber. Allocation is a single upsert-and-return, so concurrent
-- allocations serialize on the row rather than racing.
CREATE TABLE run_numbers (
    repo_id       INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    workflow_path TEXT NOT NULL,
    current       INTEGER NOT NULL,
    PRIMARY KEY (repo_id, workflow_path)
);

CREATE TABLE runs (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id             INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    repo_full_name      TEXT NOT NULL DEFAULT '',
    workflow_name       TEXT NOT NULL DEFAULT '',
    workflow_path       TEXT NOT NULL DEFAULT '',
    run_number          INTEGER NOT NULL DEFAULT 0,
    attempt             INTEGER NOT NULL DEFAULT 1,
    event               TEXT NOT NULL DEFAULT '',
    head_sha            TEXT NOT NULL DEFAULT '',
    head_branch         TEXT NOT NULL DEFAULT '',
    base_branch         TEXT NOT NULL DEFAULT '',
    actor               TEXT NOT NULL DEFAULT '',
    is_fork_pr          INTEGER NOT NULL DEFAULT 0 CHECK (is_fork_pr IN (0, 1)),
    approved            INTEGER NOT NULL DEFAULT 0 CHECK (approved IN (0, 1)),
    approved_by         TEXT NOT NULL DEFAULT '',
    check_suite_id      INTEGER NOT NULL DEFAULT 0,
    status              TEXT NOT NULL
                        CHECK (status IN ('queued', 'in_progress', 'completed', 'waiting')),
    conclusion          TEXT NOT NULL DEFAULT ''
                        CHECK (conclusion IN ('', 'success', 'failure', 'neutral', 'cancelled',
                                              'timed_out', 'action_required', 'skipped', 'stale',
                                              'infra_failure', 'config_error')),
    cancel_actor        TEXT NOT NULL DEFAULT ''
                        CHECK (cancel_actor IN ('', 'user', 'concurrency_group', 'timeout',
                                                'runner_lost', 'superseded_by_newer_run',
                                                'dependency_failed', 'control_plane_shutdown')),
    cancel_sentence     TEXT NOT NULL DEFAULT '',
    cancel_triggered_by TEXT NOT NULL DEFAULT '',
    event_payload       TEXT,
    inputs              TEXT NOT NULL DEFAULT '{}'
                        CHECK (json_type(inputs) = 'object'),
    created_at          TEXT NOT NULL,
    started_at          TEXT,
    completed_at        TEXT,
    -- A cancellation with no sentence is the incident this platform exists to
    -- never repeat, so the database refuses to store one.
    CONSTRAINT runs_cancel_explained
        CHECK ((cancel_actor = '') = (cancel_sentence = ''))
);

CREATE INDEX runs_repo_created_idx ON runs (repo_id, created_at DESC, id DESC);
CREATE INDEX runs_repo_head_sha_idx ON runs (repo_id, head_sha);

CREATE TABLE jobs (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id              INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    key                 TEXT NOT NULL,
    name                TEXT NOT NULL,
    matrix_key          TEXT NOT NULL DEFAULT '',
    matrix              TEXT NOT NULL DEFAULT '{}'
                        CHECK (json_type(matrix) = 'object'),
    needs               TEXT NOT NULL DEFAULT '[]'
                        CHECK (json_type(needs) = 'array'),
    labels              TEXT NOT NULL DEFAULT '[]'
                        CHECK (json_type(labels) = 'array'),
    attempt             INTEGER NOT NULL DEFAULT 1,
    max_attempts        INTEGER NOT NULL DEFAULT 1,
    status              TEXT NOT NULL
                        CHECK (status IN ('queued', 'in_progress', 'completed', 'waiting')),
    conclusion          TEXT NOT NULL DEFAULT ''
                        CHECK (conclusion IN ('', 'success', 'failure', 'neutral', 'cancelled',
                                              'timed_out', 'action_required', 'skipped', 'stale',
                                              'infra_failure', 'config_error')),
    class               TEXT NOT NULL DEFAULT ''
                        CHECK (class IN ('', 'user', 'infra', 'config')),
    cancel_actor        TEXT NOT NULL DEFAULT ''
                        CHECK (cancel_actor IN ('', 'user', 'concurrency_group', 'timeout',
                                                'runner_lost', 'superseded_by_newer_run',
                                                'dependency_failed', 'control_plane_shutdown')),
    cancel_sentence     TEXT NOT NULL DEFAULT '',
    cancel_triggered_by TEXT NOT NULL DEFAULT '',
    concurrency_group   TEXT NOT NULL DEFAULT '',
    cancel_in_progress  INTEGER NOT NULL DEFAULT 0 CHECK (cancel_in_progress IN (0, 1)),
    continue_on_error   INTEGER NOT NULL DEFAULT 0 CHECK (continue_on_error IN (0, 1)),
    timeout_minutes     INTEGER NOT NULL DEFAULT 0,
    check_run_id        INTEGER NOT NULL DEFAULT 0,
    runner_id           TEXT NOT NULL DEFAULT '',
    environment         TEXT NOT NULL DEFAULT '',
    awaiting_approval   INTEGER NOT NULL DEFAULT 0 CHECK (awaiting_approval IN (0, 1)),
    outputs             TEXT NOT NULL DEFAULT '{}'
                        CHECK (json_type(outputs) = 'object'),
    failure_explanation TEXT NOT NULL DEFAULT '',
    created_at          TEXT NOT NULL,
    queued_at           TEXT,
    started_at          TEXT,
    setup_completed_at  TEXT,
    completed_at        TEXT,
    lease_expires_at    TEXT,
    last_heartbeat_at   TEXT,
    requeue_count       INTEGER NOT NULL DEFAULT 0,
    infra_retry_count   INTEGER NOT NULL DEFAULT 0,
    classification_log  TEXT NOT NULL DEFAULT '[]'
                        CHECK (json_type(classification_log) = 'array'),
    CONSTRAINT jobs_cancel_explained
        CHECK ((cancel_actor = '') = (cancel_sentence = ''))
);

CREATE INDEX jobs_run_idx ON jobs (run_id, id);
CREATE INDEX jobs_lease_idx ON jobs (lease_expires_at) WHERE lease_expires_at IS NOT NULL;
CREATE INDEX jobs_concurrency_idx ON jobs (concurrency_group, created_at, id)
    WHERE concurrency_group <> '' AND status <> 'completed';

CREATE TABLE steps (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id            INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    attempt           INTEGER NOT NULL,
    number            INTEGER NOT NULL,
    name              TEXT NOT NULL DEFAULT '',
    step_id           TEXT NOT NULL DEFAULT '',
    status            TEXT NOT NULL
                      CHECK (status IN ('queued', 'in_progress', 'completed', 'waiting')),
    conclusion        TEXT NOT NULL DEFAULT ''
                      CHECK (conclusion IN ('', 'success', 'failure', 'neutral', 'cancelled',
                                            'timed_out', 'action_required', 'skipped', 'stale',
                                            'infra_failure', 'config_error')),
    class             TEXT NOT NULL DEFAULT ''
                      CHECK (class IN ('', 'user', 'infra', 'config')),
    exit_code         INTEGER NOT NULL DEFAULT 0,
    continue_on_error INTEGER NOT NULL DEFAULT 0 CHECK (continue_on_error IN (0, 1)),
    outputs           TEXT NOT NULL DEFAULT '{}'
                      CHECK (json_type(outputs) = 'object'),
    started_at        TEXT,
    completed_at      TEXT,
    log_start         INTEGER NOT NULL DEFAULT 0,
    log_end           INTEGER NOT NULL DEFAULT 0,
    UNIQUE (job_id, attempt, number)
);

-- job_queue is the durable queue. One row per job that is waiting or leased;
-- the row is deleted when the job completes, so the queue never accumulates
-- finished work.
CREATE TABLE job_queue (
    job_id            INTEGER PRIMARY KEY REFERENCES jobs(id) ON DELETE CASCADE,
    run_id            INTEGER NOT NULL,
    attempt           INTEGER NOT NULL,
    labels            TEXT NOT NULL DEFAULT '[]'
                      CHECK (json_type(labels) = 'array'),
    concurrency_group TEXT NOT NULL DEFAULT '',
    priority          INTEGER NOT NULL DEFAULT 0,
    queued_at         TEXT NOT NULL,
    -- not_before carries a retry's backoff delay; a job is invisible to
    -- Dequeue until then.
    not_before        TEXT NOT NULL,
    state             TEXT NOT NULL CHECK (state IN ('queued', 'leased')),
    runner_id         TEXT NOT NULL DEFAULT '',
    lease_expires_at  TEXT,
    requeue_count     INTEGER NOT NULL DEFAULT 0,
    CONSTRAINT job_queue_lease_consistent
        CHECK ((state = 'leased') = (lease_expires_at IS NOT NULL)
               AND (state = 'leased') = (runner_id <> ''))
);

-- job_dispatches is the idempotency record. The primary key is the whole point:
-- a control-plane restart between handing out a lease and recording it can
-- never produce a second dispatch of the same (run, job, attempt).
CREATE TABLE job_dispatches (
    run_id              INTEGER NOT NULL,
    job_id              INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    attempt             INTEGER NOT NULL,
    runner_id           TEXT NOT NULL,
    first_dispatched_at TEXT NOT NULL,
    last_dispatched_at  TEXT NOT NULL,
    -- dispatch_count > 1 means the job was redelivered after a lease loss. It
    -- is a requeue of the same dispatch, never a new one.
    dispatch_count      INTEGER NOT NULL DEFAULT 1,
    PRIMARY KEY (run_id, job_id, attempt)
);

CREATE TABLE runners (
    id             TEXT PRIMARY KEY,
    name           TEXT NOT NULL DEFAULT '',
    labels         TEXT NOT NULL DEFAULT '[]'
                   CHECK (json_type(labels) = 'array'),
    runner_group   TEXT NOT NULL DEFAULT '',
    state          TEXT NOT NULL
                   CHECK (state IN ('idle', 'busy', 'offline', 'drained')),
    current_job_id INTEGER NOT NULL DEFAULT 0,
    capacity       INTEGER NOT NULL DEFAULT 1,
    version        TEXT NOT NULL DEFAULT '',
    os             TEXT NOT NULL DEFAULT '',
    arch           TEXT NOT NULL DEFAULT '',
    first_seen_at  TEXT NOT NULL,
    last_heartbeat TEXT NOT NULL
);

CREATE INDEX runners_state_idx ON runners (state);
CREATE INDEX runners_heartbeat_idx ON runners (last_heartbeat) WHERE state <> 'offline';

CREATE TABLE annotations (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id       INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    path         TEXT NOT NULL DEFAULT '',
    start_line   INTEGER NOT NULL DEFAULT 0,
    end_line     INTEGER NOT NULL DEFAULT 0,
    start_column INTEGER NOT NULL DEFAULT 0,
    end_column   INTEGER NOT NULL DEFAULT 0,
    level        TEXT NOT NULL CHECK (level IN ('notice', 'warning', 'failure')),
    message      TEXT NOT NULL DEFAULT '',
    title        TEXT NOT NULL DEFAULT '',
    raw_details  TEXT NOT NULL DEFAULT ''
);

CREATE INDEX annotations_job_idx ON annotations (job_id, id);

CREATE TABLE artifacts (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id       INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    job_id       INTEGER NOT NULL DEFAULT 0,
    name         TEXT NOT NULL,
    size_bytes   INTEGER NOT NULL DEFAULT 0,
    digest       TEXT NOT NULL DEFAULT '',
    storage_key  TEXT NOT NULL DEFAULT '',
    expires_at   TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    finalized    INTEGER NOT NULL DEFAULT 0 CHECK (finalized IN (0, 1)),
    finalized_at TEXT,
    UNIQUE (run_id, name)
);

CREATE INDEX artifacts_expiry_idx ON artifacts (expires_at);

CREATE TABLE cache_entries (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id       INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    key           TEXT NOT NULL,
    version       TEXT NOT NULL DEFAULT '',
    ref           TEXT NOT NULL DEFAULT '',
    size_bytes    INTEGER NOT NULL DEFAULT 0,
    storage_key   TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL,
    last_accessed TEXT NOT NULL,
    finalized     INTEGER NOT NULL DEFAULT 0 CHECK (finalized IN (0, 1))
);

CREATE INDEX cache_entries_repo_key_idx ON cache_entries (repo_id, key);
CREATE INDEX cache_entries_lru_idx ON cache_entries (repo_id, last_accessed) WHERE finalized = 1;

CREATE TABLE cache_events (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id    INTEGER NOT NULL,
    key        TEXT NOT NULL DEFAULT '',
    kind       TEXT NOT NULL CHECK (kind IN ('hit', 'miss', 'store', 'evict')),
    matched_on TEXT NOT NULL DEFAULT '',
    reason     TEXT NOT NULL DEFAULT '',
    size_bytes INTEGER NOT NULL DEFAULT 0,
    at         TEXT NOT NULL
);

CREATE INDEX cache_events_repo_idx ON cache_events (repo_id, at DESC, id DESC);

CREATE TABLE secrets (
    scope      TEXT NOT NULL CHECK (scope IN ('org', 'repo', 'environment')),
    scope_key  TEXT NOT NULL,
    name       TEXT NOT NULL,
    ciphertext BLOB NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (scope, scope_key, name)
);

CREATE TABLE vars (
    scope      TEXT NOT NULL CHECK (scope IN ('org', 'repo', 'environment')),
    scope_key  TEXT NOT NULL,
    name       TEXT NOT NULL,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (scope, scope_key, name)
);

-- events is the audit trail read back as a job's timeline. run_id/job_id are 0
-- when the event is not scoped to one, so they carry no foreign key.
CREATE TABLE events (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id  INTEGER NOT NULL DEFAULT 0,
    job_id  INTEGER NOT NULL DEFAULT 0,
    kind    TEXT NOT NULL,
    message TEXT NOT NULL DEFAULT '',
    detail  TEXT NOT NULL DEFAULT '{}'
            CHECK (json_type(detail) = 'object'),
    at      TEXT NOT NULL
);

CREATE INDEX events_job_idx ON events (job_id, id) WHERE job_id <> 0;
CREATE INDEX events_run_idx ON events (run_id, id) WHERE run_id <> 0;

CREATE TABLE queue_samples (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    at             TEXT NOT NULL,
    depth          INTEGER NOT NULL DEFAULT 0,
    depth_by_label TEXT NOT NULL DEFAULT '{}'
                   CHECK (json_type(depth_by_label) = 'object'),
    busy           INTEGER NOT NULL DEFAULT 0,
    idle           INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX queue_samples_at_idx ON queue_samples (at, id);
-- Indexes for the dequeue hot path.
--
-- Dequeue picks the first eligible row in (priority DESC, queued_at ASC) order
-- inside an immediate transaction. The partial index keeps leased rows out of
-- that scan entirely, so a queue full of running work costs nothing to poll.
CREATE INDEX job_queue_dequeue_idx
    ON job_queue (priority DESC, queued_at ASC, job_id)
    WHERE state = 'queued';

-- Label matching walks the job's labels with json_each and checks each against
-- the runner's set, so there is no index to build: the candidate rows are
-- already narrowed by job_queue_dequeue_idx before any label is read.

-- The reaper looks up expired leases only.
CREATE INDEX job_queue_lease_expiry_idx
    ON job_queue (lease_expires_at)
    WHERE state = 'leased';

CREATE INDEX job_queue_run_idx ON job_queue (run_id);
