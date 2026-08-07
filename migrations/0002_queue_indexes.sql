-- Indexes for the dequeue hot path.
--
-- Dequeue scans eligible rows in (priority DESC, queued_at ASC) order under
-- FOR UPDATE SKIP LOCKED. The partial index keeps leased rows out of that scan
-- entirely, so a queue full of running work costs nothing to poll.
CREATE INDEX job_queue_dequeue_idx
    ON job_queue (priority DESC, queued_at ASC, job_id)
    WHERE state = 'queued';

-- Label matching is jsonb array containment (runner labels @> job labels).
CREATE INDEX job_queue_labels_idx
    ON job_queue USING gin (labels jsonb_path_ops);

-- The reaper looks up expired leases only.
CREATE INDEX job_queue_lease_expiry_idx
    ON job_queue (lease_expires_at)
    WHERE state = 'leased';

CREATE INDEX job_queue_run_idx ON job_queue (run_id);
