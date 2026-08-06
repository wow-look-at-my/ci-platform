-- Initial schema: repos, runs, jobs, steps, the durable queue and its lease
-- protocol, runners, annotations, artifacts, caches, secrets, vars, events.
--
-- Conventions:
--   * every timestamp is timestamptz; naive local times are never stored
--   * every enum-ish column is text with a CHECK listing the valid values, so a
--     typo fails at write time instead of reading back as something else
--   * every structured column is jsonb, with a jsonb_typeof CHECK where the
--     shape (array vs object) is load-bearing

CREATE TABLE repos (
    id              bigint PRIMARY KEY,
    owner           text NOT NULL,
    name            text NOT NULL,
    installation_id bigint NOT NULL DEFAULT 0,
    default_branch  text NOT NULL DEFAULT '',
    private         boolean NOT NULL DEFAULT false,
    UNIQUE (owner, name)
);

-- run_numbers is the per-(repo, workflow) monotonic counter behind
-- NextRunNumber. Allocation is a single upsert-and-return, so concurrent
-- allocations serialize on the row rather than racing.
CREATE TABLE run_numbers (
    repo_id       bigint NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    workflow_path text NOT NULL,
    current       bigint NOT NULL,
    PRIMARY KEY (repo_id, workflow_path)
);

CREATE TABLE runs (
    id                  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    repo_id             bigint NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    repo_full_name      text NOT NULL DEFAULT '',
    workflow_name       text NOT NULL DEFAULT '',
    workflow_path       text NOT NULL DEFAULT '',
    run_number          bigint NOT NULL DEFAULT 0,
    attempt             int NOT NULL DEFAULT 1,
    event               text NOT NULL DEFAULT '',
    head_sha            text NOT NULL DEFAULT '',
    head_branch         text NOT NULL DEFAULT '',
    base_branch         text NOT NULL DEFAULT '',
    actor               text NOT NULL DEFAULT '',
    is_fork_pr          boolean NOT NULL DEFAULT false,
    approved            boolean NOT NULL DEFAULT false,
    approved_by         text NOT NULL DEFAULT '',
    check_suite_id      bigint NOT NULL DEFAULT 0,
    status              text NOT NULL
                        CHECK (status IN ('queued', 'in_progress', 'completed', 'waiting')),
    conclusion          text NOT NULL DEFAULT ''
                        CHECK (conclusion IN ('', 'success', 'failure', 'neutral', 'cancelled',
                                              'timed_out', 'action_required', 'skipped', 'stale',
                                              'infra_failure', 'config_error')),
    cancel_actor        text NOT NULL DEFAULT ''
                        CHECK (cancel_actor IN ('', 'user', 'concurrency_group', 'timeout',
                                                'runner_lost', 'superseded_by_newer_run',
                                                'dependency_failed', 'control_plane_shutdown')),
    cancel_sentence     text NOT NULL DEFAULT '',
    cancel_triggered_by text NOT NULL DEFAULT '',
    event_payload       jsonb,
    inputs              jsonb NOT NULL DEFAULT '{}'::jsonb
                        CHECK (jsonb_typeof(inputs) = 'object'),
    created_at          timestamptz NOT NULL,
    started_at          timestamptz,
    completed_at        timestamptz,
    -- A cancellation with no sentence is the incident this platform exists to
    -- never repeat, so the database refuses to store one.
    CONSTRAINT runs_cancel_explained
        CHECK ((cancel_actor = '') = (cancel_sentence = ''))
);

CREATE INDEX runs_repo_created_idx ON runs (repo_id, created_at DESC, id DESC);
CREATE INDEX runs_repo_head_sha_idx ON runs (repo_id, head_sha);

CREATE TABLE jobs (
    id                  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_id              bigint NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    key                 text NOT NULL,
    name                text NOT NULL,
    matrix_key          text NOT NULL DEFAULT '',
    matrix              jsonb NOT NULL DEFAULT '{}'::jsonb
                        CHECK (jsonb_typeof(matrix) = 'object'),
    needs               jsonb NOT NULL DEFAULT '[]'::jsonb
                        CHECK (jsonb_typeof(needs) = 'array'),
    labels              jsonb NOT NULL DEFAULT '[]'::jsonb
                        CHECK (jsonb_typeof(labels) = 'array'),
    attempt             int NOT NULL DEFAULT 1,
    max_attempts        int NOT NULL DEFAULT 1,
    status              text NOT NULL
                        CHECK (status IN ('queued', 'in_progress', 'completed', 'waiting')),
    conclusion          text NOT NULL DEFAULT ''
                        CHECK (conclusion IN ('', 'success', 'failure', 'neutral', 'cancelled',
                                              'timed_out', 'action_required', 'skipped', 'stale',
                                              'infra_failure', 'config_error')),
    class               text NOT NULL DEFAULT ''
                        CHECK (class IN ('', 'user', 'infra', 'config')),
    cancel_actor        text NOT NULL DEFAULT ''
                        CHECK (cancel_actor IN ('', 'user', 'concurrency_group', 'timeout',
                                                'runner_lost', 'superseded_by_newer_run',
                                                'dependency_failed', 'control_plane_shutdown')),
    cancel_sentence     text NOT NULL DEFAULT '',
    cancel_triggered_by text NOT NULL DEFAULT '',
    concurrency_group   text NOT NULL DEFAULT '',
    cancel_in_progress  boolean NOT NULL DEFAULT false,
    continue_on_error   boolean NOT NULL DEFAULT false,
    timeout_minutes     int NOT NULL DEFAULT 0,
    check_run_id        bigint NOT NULL DEFAULT 0,
    runner_id           text NOT NULL DEFAULT '',
    environment         text NOT NULL DEFAULT '',
    awaiting_approval   boolean NOT NULL DEFAULT false,
    outputs             jsonb NOT NULL DEFAULT '{}'::jsonb
                        CHECK (jsonb_typeof(outputs) = 'object'),
    failure_explanation text NOT NULL DEFAULT '',
    created_at          timestamptz NOT NULL,
    queued_at           timestamptz,
    started_at          timestamptz,
    setup_completed_at  timestamptz,
    completed_at        timestamptz,
    lease_expires_at    timestamptz,
    last_heartbeat_at   timestamptz,
    requeue_count       int NOT NULL DEFAULT 0,
    infra_retry_count   int NOT NULL DEFAULT 0,
    classification_log  jsonb NOT NULL DEFAULT '[]'::jsonb
                        CHECK (jsonb_typeof(classification_log) = 'array'),
    CONSTRAINT jobs_cancel_explained
        CHECK ((cancel_actor = '') = (cancel_sentence = ''))
);

CREATE INDEX jobs_run_idx ON jobs (run_id, id);
CREATE INDEX jobs_lease_idx ON jobs (lease_expires_at) WHERE lease_expires_at IS NOT NULL;
CREATE INDEX jobs_concurrency_idx ON jobs (concurrency_group, created_at, id)
    WHERE concurrency_group <> '' AND status <> 'completed';

CREATE TABLE steps (
    id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    job_id            bigint NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    attempt           int NOT NULL,
    number            int NOT NULL,
    name              text NOT NULL DEFAULT '',
    step_id           text NOT NULL DEFAULT '',
    status            text NOT NULL
                      CHECK (status IN ('queued', 'in_progress', 'completed', 'waiting')),
    conclusion        text NOT NULL DEFAULT ''
                      CHECK (conclusion IN ('', 'success', 'failure', 'neutral', 'cancelled',
                                            'timed_out', 'action_required', 'skipped', 'stale',
                                            'infra_failure', 'config_error')),
    class             text NOT NULL DEFAULT ''
                      CHECK (class IN ('', 'user', 'infra', 'config')),
    exit_code         int NOT NULL DEFAULT 0,
    continue_on_error boolean NOT NULL DEFAULT false,
    outputs           jsonb NOT NULL DEFAULT '{}'::jsonb
                      CHECK (jsonb_typeof(outputs) = 'object'),
    started_at        timestamptz,
    completed_at      timestamptz,
    log_start         bigint NOT NULL DEFAULT 0,
    log_end           bigint NOT NULL DEFAULT 0,
    UNIQUE (job_id, attempt, number)
);

-- job_queue is the durable queue. One row per job that is waiting or leased;
-- the row is deleted when the job completes, so the queue never accumulates
-- finished work.
CREATE TABLE job_queue (
    job_id            bigint PRIMARY KEY REFERENCES jobs(id) ON DELETE CASCADE,
    run_id            bigint NOT NULL,
    attempt           int NOT NULL,
    labels            jsonb NOT NULL DEFAULT '[]'::jsonb
                      CHECK (jsonb_typeof(labels) = 'array'),
    concurrency_group text NOT NULL DEFAULT '',
    priority          int NOT NULL DEFAULT 0,
    queued_at         timestamptz NOT NULL,
    -- not_before carries a retry's backoff delay; a job is invisible to
    -- Dequeue until then.
    not_before        timestamptz NOT NULL,
    state             text NOT NULL CHECK (state IN ('queued', 'leased')),
    runner_id         text NOT NULL DEFAULT '',
    lease_expires_at  timestamptz,
    requeue_count     int NOT NULL DEFAULT 0,
    CONSTRAINT job_queue_lease_consistent
        CHECK ((state = 'leased') = (lease_expires_at IS NOT NULL)
               AND (state = 'leased') = (runner_id <> ''))
);

-- job_dispatches is the idempotency record. The primary key is the whole point:
-- a control-plane restart between handing out a lease and recording it can
-- never produce a second dispatch of the same (run, job, attempt).
CREATE TABLE job_dispatches (
    run_id              bigint NOT NULL,
    job_id              bigint NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    attempt             int NOT NULL,
    runner_id           text NOT NULL,
    first_dispatched_at timestamptz NOT NULL,
    last_dispatched_at  timestamptz NOT NULL,
    -- dispatch_count > 1 means the job was redelivered after a lease loss. It
    -- is a requeue of the same dispatch, never a new one.
    dispatch_count      int NOT NULL DEFAULT 1,
    PRIMARY KEY (run_id, job_id, attempt)
);

CREATE TABLE runners (
    id             text PRIMARY KEY,
    name           text NOT NULL DEFAULT '',
    labels         jsonb NOT NULL DEFAULT '[]'::jsonb
                   CHECK (jsonb_typeof(labels) = 'array'),
    runner_group   text NOT NULL DEFAULT '',
    state          text NOT NULL
                   CHECK (state IN ('idle', 'busy', 'offline', 'drained')),
    current_job_id bigint NOT NULL DEFAULT 0,
    capacity       int NOT NULL DEFAULT 1,
    version        text NOT NULL DEFAULT '',
    os             text NOT NULL DEFAULT '',
    arch           text NOT NULL DEFAULT '',
    first_seen_at  timestamptz NOT NULL,
    last_heartbeat timestamptz NOT NULL
);

CREATE INDEX runners_state_idx ON runners (state);
CREATE INDEX runners_heartbeat_idx ON runners (last_heartbeat) WHERE state <> 'offline';

CREATE TABLE annotations (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    job_id       bigint NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    path         text NOT NULL DEFAULT '',
    start_line   int NOT NULL DEFAULT 0,
    end_line     int NOT NULL DEFAULT 0,
    start_column int NOT NULL DEFAULT 0,
    end_column   int NOT NULL DEFAULT 0,
    level        text NOT NULL CHECK (level IN ('notice', 'warning', 'failure')),
    message      text NOT NULL DEFAULT '',
    title        text NOT NULL DEFAULT '',
    raw_details  text NOT NULL DEFAULT ''
);

CREATE INDEX annotations_job_idx ON annotations (job_id, id);

CREATE TABLE artifacts (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_id       bigint NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    job_id       bigint NOT NULL DEFAULT 0,
    name         text NOT NULL,
    size_bytes   bigint NOT NULL DEFAULT 0,
    digest       text NOT NULL DEFAULT '',
    storage_key  text NOT NULL DEFAULT '',
    expires_at   timestamptz NOT NULL,
    created_at   timestamptz NOT NULL,
    finalized    boolean NOT NULL DEFAULT false,
    finalized_at timestamptz,
    UNIQUE (run_id, name)
);

CREATE INDEX artifacts_expiry_idx ON artifacts (expires_at);

CREATE TABLE cache_entries (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    repo_id       bigint NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    key           text NOT NULL,
    version       text NOT NULL DEFAULT '',
    ref           text NOT NULL DEFAULT '',
    size_bytes    bigint NOT NULL DEFAULT 0,
    storage_key   text NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL,
    last_accessed timestamptz NOT NULL,
    finalized     boolean NOT NULL DEFAULT false
);

CREATE INDEX cache_entries_repo_key_idx ON cache_entries (repo_id, key);
CREATE INDEX cache_entries_lru_idx ON cache_entries (repo_id, last_accessed) WHERE finalized;

CREATE TABLE cache_events (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    repo_id    bigint NOT NULL,
    key        text NOT NULL DEFAULT '',
    kind       text NOT NULL CHECK (kind IN ('hit', 'miss', 'store', 'evict')),
    matched_on text NOT NULL DEFAULT '',
    reason     text NOT NULL DEFAULT '',
    size_bytes bigint NOT NULL DEFAULT 0,
    at         timestamptz NOT NULL
);

CREATE INDEX cache_events_repo_idx ON cache_events (repo_id, at DESC, id DESC);

CREATE TABLE secrets (
    scope      text NOT NULL CHECK (scope IN ('org', 'repo', 'environment')),
    scope_key  text NOT NULL,
    name       text NOT NULL,
    ciphertext bytea NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (scope, scope_key, name)
);

CREATE TABLE vars (
    scope      text NOT NULL CHECK (scope IN ('org', 'repo', 'environment')),
    scope_key  text NOT NULL,
    name       text NOT NULL,
    value      text NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (scope, scope_key, name)
);

-- events is the audit trail read back as a job's timeline. run_id/job_id are 0
-- when the event is not scoped to one, so they carry no foreign key.
CREATE TABLE events (
    id      bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_id  bigint NOT NULL DEFAULT 0,
    job_id  bigint NOT NULL DEFAULT 0,
    kind    text NOT NULL,
    message text NOT NULL DEFAULT '',
    detail  jsonb NOT NULL DEFAULT '{}'::jsonb
            CHECK (jsonb_typeof(detail) = 'object'),
    at      timestamptz NOT NULL
);

CREATE INDEX events_job_idx ON events (job_id, id) WHERE job_id <> 0;
CREATE INDEX events_run_idx ON events (run_id, id) WHERE run_id <> 0;

CREATE TABLE queue_samples (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    at             timestamptz NOT NULL,
    depth          int NOT NULL DEFAULT 0,
    depth_by_label jsonb NOT NULL DEFAULT '{}'::jsonb
                   CHECK (jsonb_typeof(depth_by_label) = 'object'),
    busy           int NOT NULL DEFAULT 0,
    idle           int NOT NULL DEFAULT 0
);

CREATE INDEX queue_samples_at_idx ON queue_samples (at, id);
