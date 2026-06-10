-- runq SQLite schema

CREATE TABLE IF NOT EXISTS projects (
    name         TEXT PRIMARY KEY,
    config_json  TEXT NOT NULL,       -- serialized project.Config as JSON
    created_at   DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at   DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS jobs (
    id           TEXT PRIMARY KEY,    -- ULID or short UUID
    project_name TEXT NOT NULL REFERENCES projects(name),
    description  TEXT,
    note         TEXT,                -- user-supplied experiment note (--note flag or job.yaml note: field)
    config_json  TEXT NOT NULL,       -- serialized job.JobConfig as JSON
    status       TEXT NOT NULL DEFAULT 'pending',  -- pending/running/paused/done
    total_tasks  INTEGER NOT NULL DEFAULT 0,
    created_at   INTEGER,             -- Unix timestamp
    finished_at  INTEGER,             -- Unix timestamp, nullable
    refreshed_at INTEGER              -- Unix timestamp, nullable. Last reconcile from external sources (HPC mode only; hpc.Refresh is the sole writer)
);

CREATE TABLE IF NOT EXISTS tasks (
    id           TEXT PRIMARY KEY,    -- ULID or short UUID
    job_id       TEXT NOT NULL REFERENCES jobs(id),
    project_name TEXT NOT NULL,
    command      TEXT NOT NULL,
    params_json  TEXT NOT NULL,       -- serialized TaskParams as JSON
    gpus_needed  INTEGER NOT NULL DEFAULT 1,
    gpus         TEXT,                -- comma-separated GPU indices, e.g. "0,1,3"
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
CREATE INDEX IF NOT EXISTS idx_metrics_job_key  ON metrics(job_id, key);
CREATE INDEX IF NOT EXISTS idx_metrics_task_key ON metrics(task_id, key, step);

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
