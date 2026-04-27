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
    config_json  TEXT NOT NULL,       -- serialized job.JobConfig as JSON
    status       TEXT NOT NULL DEFAULT 'pending',  -- pending/running/paused/done
    total_tasks  INTEGER NOT NULL DEFAULT 0,
    created_at   INTEGER,             -- Unix timestamp
    finished_at  INTEGER              -- Unix timestamp, nullable
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
    enqueued_at  INTEGER,            -- Unix timestamp
    started_at   INTEGER,            -- Unix timestamp, nullable
    finished_at  INTEGER             -- Unix timestamp, nullable
);

CREATE INDEX IF NOT EXISTS idx_tasks_job_id      ON tasks(job_id);
CREATE INDEX IF NOT EXISTS idx_tasks_status      ON tasks(status);
CREATE INDEX IF NOT EXISTS idx_tasks_finished_at ON tasks(finished_at);
CREATE INDEX IF NOT EXISTS idx_jobs_status       ON jobs(status);
CREATE INDEX IF NOT EXISTS idx_jobs_finished_at  ON jobs(finished_at);
