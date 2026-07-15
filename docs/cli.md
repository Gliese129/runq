# CLI reference

Conventions: job ids look like `jb…`, task ids like `tk…` — take them from
`--json` output rather than parsing tables. Every read command supports
`--json`. `-t <target>` routes any command to an HPC target; `--fresh`
forces a cache refresh (rate-limited server-side). Errors are documentation:
they state what's wrong and how to fix it, and exit non-zero.

## Core

| Command | What it does |
|---|---|
| `runq init [script.py]` | Scan argparse → generate project.yaml + job.yaml; auto-detect uv/venv/conda |
| `runq submit <job.yaml \| .>` | Submit a job. `--dry` preview (nothing submitted) · `--watch` live progress · `--note` label · `--project` / `--project-file` override · `--json` → `{job_id, total_tasks}` |
| `runq sweep k=v1,v2 …` | Quick sweep, no YAML. **Submits immediately** unless `--dry`. `--list` = zip 1-to-1 instead of grid · `--project` (default: cwd name) · `-n` note · `--gpus` · `--max-retry` |
| `runq run <project> [--gpus N] -- <args…>` | One-off single task |

## Monitoring

| Command | What it does |
|---|---|
| `runq ps` (alias `ls`) | Job table: id, status, done/total, note |
| `runq ps <job_id>` | That job's tasks with params |
| `runq status` | Daemon / queue summary |
| `runq status <job_id>` | Refresh + show one job's tasks |
| `runq gpu` | Per-GPU allocation on this machine |
| `runq logs <task_id>` | Tail + **follow** (blocks). `--no-follow` prints and exits · `-n N` tail size |
| `runq best <job_id> --key <m>` | Best task by metric. Lower-is-better by default; `--max` for accuracy-style |
| `runq collect <job_id> --key <m>` | All tasks ranked by metric (`--max`, `--json`) |
| `runq task show <task_id>` | Params, status, GPU, retry count, log path |

## Control

| Command | What it does |
|---|---|
| `runq kill <task_id \| job_id>` | Kill a task, or every task in a job |
| `runq task retry <task_id>` | Requeue a failed task |
| `runq job pause / resume <job_id>` | Stop / resume dispatching (running tasks continue) |
| `runq job archive / unarchive <job_id>` | Hide from default lists — reversible, data untouched |
| `runq project archive / unarchive <name>` | Same, for a project and its jobs |

## Projects

| Command | What it does |
|---|---|
| `runq project add <name \| .>` | Register a project (reads ./project.yaml) |
| `runq project ls / show / edit` | List, inspect, or open in `$EDITOR` |

project.yaml is the source of truth: hand-edits are picked up on next use.
There is no re-registration step.

## Daemon (local machines)

| Command | What it does |
|---|---|
| `runq daemon start [-d]` | Start the scheduler daemon (`-d` = background) |
| `runq daemon stop / restart` | Stop / restart (restart always backgrounds) |
| `runq doctor` | Static self-check of this machine, with fixes |

## HPC targets

| Command | What it does |
|---|---|
| `runq target add <name> --template=<slurm\|pbs\|sge\|tsubame\|abci> --host=… --user=…` | Add a target from a preset |
| `runq target check [<name>]` | Validate templates: placeholders, regex, sample render — free |
| `runq target show / edit` | Inspect, or open config.yaml in `$EDITOR` + validate |
| `runq connect <name>` | Verify SSH + host key, install remote CLI, start forward |
| `runq target disconnect <name>` | Stop the forward, disable remote_cli |
| `runq target use <name> [-p]` | Select the active target for this session (`-p` = persist) |

## Housekeeping

| Command | What it does |
|---|---|
| `runq clean` | Delete finished tasks **and all their artifacts — irreversible**. Interactive; `--show` previews without deleting · `-y` skips confirmation · `--older-than 720h` · `--orphan` · `--archived` |
| `runq thaw` | Release tasks the SDK SIGSTOPped on low disk |
| `runq config get / set / list` | Global config keys (e.g. `data_path`, `default_target`) |
| `runq version` | Build version |

## Plumbing (you rarely need these)

`runq sbatch / squeue / scancel` — foreign-task preset plumbing used by the
runq scheduler preset · `runq metrics-index` — build/inspect the metrics
pyramid index · `--socket` — non-default daemon socket path.

## Notes for scripts and agents

- `runq sweep` without `--dry` mutates the queue — never use it to
  "explore" the CLI.
- `runq logs` follows by default and will block a non-interactive caller;
  always pass `--no-follow` in scripts.
- `runq clean` is irreversible; preview with `--show` first.
- Machine-readable state: `runq ps --json`, `runq ps <job_id> --json`,
  `runq status <job_id> --json`, `runq submit … --json`.
