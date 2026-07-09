-- runq SQLite schema

CREATE TABLE IF NOT EXISTS projects (
    name         TEXT PRIMARY KEY,
    config_json  TEXT NOT NULL,       -- serialized project.Config as JSON
    created_at   DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at   DATETIME DEFAULT CURRENT_TIMESTAMP,
    archived_at  INTEGER              -- Unix timestamp, nullable. Archived = hidden from default lists; data untouched
);

CREATE TABLE IF NOT EXISTS jobs (
    id           TEXT PRIMARY KEY,    -- ULID or short UUID
    -- No FK to projects: on runqd this ledger is a SCHEDULER's — foreign
    -- tasks carry the client's project name as a plain LABEL (like Slurm
    -- records a job name); registration is a client-side concept. Pre-split
    -- client DBs keep their old FK (CREATE IF NOT EXISTS never alters) and
    -- the client only writes registered projects, so behavior is identical.
    project_name TEXT NOT NULL,
    description  TEXT,
    note         TEXT,                -- user-supplied experiment note (--note flag or job.yaml note: field)
    config_json  TEXT NOT NULL,       -- serialized job.JobConfig as JSON
    status       TEXT NOT NULL DEFAULT 'pending',  -- pending/running/paused/done
    total_tasks  INTEGER NOT NULL DEFAULT 0,
    target       TEXT NOT NULL DEFAULT 'local',    -- compute target name (Phase 1: MultiBackend routing)
    created_at   INTEGER,             -- Unix timestamp
    finished_at  INTEGER,             -- Unix timestamp, nullable
    refreshed_at INTEGER,             -- Unix timestamp, nullable. Last reconcile from external sources (HPC mode only; hpc.Refresh is the sole writer)
    archived_at  INTEGER              -- Unix timestamp, nullable. Archived = hidden from default lists; data untouched
);

CREATE TABLE IF NOT EXISTS tasks (
    id           TEXT PRIMARY KEY,    -- ULID or short UUID
    job_id       TEXT NOT NULL REFERENCES jobs(id),
    project_name TEXT NOT NULL,
    command      TEXT NOT NULL,
    params_json  TEXT NOT NULL,       -- serialized TaskParams as JSON
    gpus_needed  INTEGER NOT NULL DEFAULT 1,
    gpus         TEXT,                -- comma-separated GPU indices, e.g. "0,1,3"
    target       TEXT NOT NULL DEFAULT 'local',    -- compute target name (Phase 1: MultiBackend routing)
    status       TEXT NOT NULL DEFAULT 'pending',
    retry_count  INTEGER NOT NULL DEFAULT 0,
    max_retry    INTEGER NOT NULL DEFAULT 0,  -- 0 = unlimited
    pid          INTEGER,
    start_time   INTEGER,            -- /proc starttime for reclaim validation
    log_path     TEXT,
    working_dir  TEXT,               -- working directory for the task
    env_json     TEXT,               -- environment variables as JSON, e.g. {"WANDB_PROJECT":"myproj"}
    resumable    INTEGER NOT NULL DEFAULT 0,  -- 0=false, 1=true
    extra_args   TEXT DEFAULT '',     -- extra args appended to command on resume
    uid          INTEGER,            -- submitting user's UID (os.Getuid)
    timeout      INTEGER,            -- task timeout in seconds, nullable (0 = no timeout)
    enqueued_at  INTEGER,            -- Unix timestamp
    started_at   INTEGER,            -- Unix timestamp, nullable
    finished_at  INTEGER,            -- Unix timestamp, nullable
    task_dir     TEXT,                -- L2-C: <root>/<job_id>/<task_id>, holds params.json / metrics.jsonl / checkpoints/
    external_id  TEXT,                -- L2-E: HPC scheduler job id (sbatch/qsub). Empty for daemon-managed tasks.
    status_source TEXT                -- L2-E: provenance of status: "" | wrapper | scheduler | inferred | runq | submit
);

CREATE INDEX IF NOT EXISTS idx_tasks_job_id      ON tasks(job_id);
CREATE INDEX IF NOT EXISTS idx_tasks_status      ON tasks(status);
CREATE INDEX IF NOT EXISTS idx_tasks_finished_at ON tasks(finished_at);
CREATE INDEX IF NOT EXISTS idx_jobs_status       ON jobs(status);
CREATE INDEX IF NOT EXISTS idx_jobs_finished_at  ON jobs(finished_at);

-- ── L2-C: metrics and checkpoints ─────────────────────────────────────────
-- Populated by daemon during `runTask` reap: read <task_dir>/metrics.jsonl,
-- dispatch each event by type (metric / checkpoint / disk_low).
-- Both tables carry job_id as a redundant column to avoid joining on `tasks`
-- when querying by job (e.g. `runq log search --job <id> --key loss`).

CREATE TABLE IF NOT EXISTS metrics (
    task_id  TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    job_id   TEXT NOT NULL,                  -- denormalized for fast filtering
    key      TEXT NOT NULL,                  -- e.g. "loss" or "train/loss"
    value    REAL,
    step     INTEGER,                        -- nullable: scripts that don't track step
    ts       INTEGER NOT NULL,               -- Unix timestamp from SDK at log time
    PRIMARY KEY (task_id, key, step, ts)
);
-- NOTE(pyramid pivot): the metrics point table above is LEGACY — the
-- streaming-reduction design stores no raw points (metric_summary below +
-- the on-target metrics.pyr index replace it). Kept so existing DBs open
-- cleanly; drop in the dead-code cleanup batch alongside its indexes.
CREATE INDEX IF NOT EXISTS idx_metrics_job_key  ON metrics(job_id, key);
CREATE INDEX IF NOT EXISTS idx_metrics_task_key ON metrics(task_id, key, step);

-- Streaming metric summaries: ONE row per (task, key), maintained by the
-- incremental ingest's on-the-fly reduction — raw points are NOT stored
-- (a 3-day chatty task is 10M+ points; charts read the raw tail window or
-- the on-target pyramid index instead). min/max/count merge losslessly
-- across delta passes; best/collect/leaderboard are O(keys) lookups.
CREATE TABLE IF NOT EXISTS metric_summary (
    task_id  TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    job_id   TEXT NOT NULL,
    key      TEXT NOT NULL,
    min      REAL NOT NULL,
    max      REAL NOT NULL,
    last     REAL NOT NULL,
    last_ts  INTEGER NOT NULL,
    count    INTEGER NOT NULL,
    PRIMARY KEY (task_id, key)
);
CREATE INDEX IF NOT EXISTS idx_metric_summary_job ON metric_summary(job_id, key);

-- Incremental ingest marks (spec §8.1.4): (size, parsed_offset) instead of
-- a hash — hashing means reading the whole file, wasted IO. size grew →
-- parse from parsed_offset; size shrank (retry rerun rewrote the file) →
-- full rebuild. final=1 freezes the mark after the terminal pass: settled
-- tasks are never stat'ed again.
CREATE TABLE IF NOT EXISTS metrics_ingest (
    task_id       TEXT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
    file_size     INTEGER NOT NULL DEFAULT 0,  -- stat size at last pass
    parsed_offset INTEGER NOT NULL DEFAULT 0,  -- consumed up to here (complete lines only)
    final         INTEGER NOT NULL DEFAULT 0   -- 1 = terminal ingest done
);

CREATE TABLE IF NOT EXISTS checkpoints (
    task_id     TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    job_id      TEXT NOT NULL,
    path        TEXT NOT NULL,
    size_bytes  INTEGER,
    step        INTEGER,
    is_best     INTEGER NOT NULL DEFAULT 0,  -- SQLite has no native boolean
    ts          INTEGER NOT NULL,
    PRIMARY KEY (task_id, step)
);
CREATE INDEX IF NOT EXISTS idx_checkpoints_job ON checkpoints(job_id);

-- L4 freshness ledger: one row per (target, resource) recording the sync
-- loop's last outcome. refreshed_at is a property of the DATA (the photo's
-- timestamp), not of the daemon process — so it lives here and survives
-- restarts. Process state (in-flight flags, throttle timers) stays in
-- memory. resource: 'contact' (passive reachability, D6) | 'tasks'
-- (scheduler sync). last_success only advances on error-free passes.
CREATE TABLE IF NOT EXISTS sync_state (
    target       TEXT NOT NULL,
    resource     TEXT NOT NULL,
    last_success INTEGER NOT NULL DEFAULT 0,  -- unix; 0 = never succeeded
    last_attempt INTEGER NOT NULL DEFAULT 0,  -- unix; 0 = never attempted
    last_error   TEXT NOT NULL DEFAULT '',    -- '' = last attempt succeeded
    PRIMARY KEY (target, resource)
);
