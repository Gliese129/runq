# Concepts

How runq-lab thinks about experiments, and what each piece does.

## The two programs

**`runq`** is the client. It runs on your laptop or any machine where
you manage experiments. It handles projects, sweep expansion, target
routing, and the optional web dashboard.

**`runqd`** is the executor. It runs on a Linux machine with GPUs. It
owns the queue, GPU allocation, process lifecycle, and crash recovery for
that one machine.

They communicate over a Unix socket. Neither embeds the other.

On an HPC cluster, you don't need `runqd` at all — `runq` talks to the
cluster's scheduler (SLURM, PBS, SGE) over SSH.

## Projects, jobs, and tasks

A **project** is a training script plus its parameter catalog
(`project.yaml`). It says what *can* vary: learning rate, batch size,
optimizer — with types, defaults, and constraints.

A **job** is one submission. It selects which parameters vary *this time*
and how (`job.yaml` or `runq sweep`). One job produces many tasks.

A **task** is one run with one set of parameters. Each task gets its own
workspace, log, and parameter snapshot. Tasks are the atomic unit — they
succeed, fail, get retried, or get killed independently.

```
project "resnet50"
  └── job jb_a1b2 (lr × optimizer sweep)
       ├── task tk_001 (lr=0.001, optimizer=adam)
       ├── task tk_002 (lr=0.001, optimizer=sgd)
       ├── task tk_003 (lr=0.01, optimizer=adam)
       └── ...
```

## Sweep expansion

Sweeps are described as blocks that combine:

- **grid** blocks produce the cartesian product of their parameters.
- **list** blocks zip values 1-to-1 (for values that belong together,
  like a benchmark and its walltime).
- Blocks **cross-multiply**: `grid(3×2) × list(2) = 12 tasks`.

The expansion is deterministic and previewable with `--dry`.

## Targets

A **target** is where tasks run. runq supports two kinds:

- **local** — a `runqd` instance on a GPU machine (Unix socket).
- **HPC** — a cluster login node (SSH + submit/status/kill templates).

Targets are configured in `~/.runq/config.yaml`. You can switch between
them per command (`-t name`), per session (`runq target use name`), or
persistently.

The same project and job YAML work on any target. Only the execution
backend changes.

## Configuration priority

When a parameter has multiple sources, this is the order:

```
CLI flag  >  job.yaml override  >  project.yaml default  >  built-in
```

`project.yaml` is always the source of truth for the catalog. The
database is a self-healing cache — if you edit project.yaml by hand,
the next command or dashboard load picks it up.

## Workspace and storage

Each task gets a workspace under `<working_dir>/.runq/<note>-<job_id>/<task_id>/`:

```
params.json       — exact parameters for this task
metrics.jsonl     — metric stream (log_metric / report)
results.jsonl     — result records (runq.record)
events.jsonl      — lifecycle events (checkpoint, preempt, loop_break)
checkpoints/      — saved checkpoints (safe_save)
run.sh            — HPC only: generated wrapper script
status.json       — HPC only: self-reported status
```

The job directory name starts with your `note`, so `ls .runq/` reads
like an experiment log.

## Secrets

Tokens and API keys go in `working_dir/.env`. This file is sourced at
each task's start but **never stored** in the database, logs, or any UI.
Do not put secrets in `command_template` or `environment:`.

## Dashboard

The embedded web dashboard (`runq-dashboard` build) provides:

- Job and task tables with real-time status
- Parameter sweep visualization
- Metric charts and comparison
- Project and target configuration
- HPC template editing with placeholder hints

It is optional — everything the dashboard does is also available through
the CLI.

## How the Python SDK fits in

The SDK runs *inside* your training script. It receives parameters from
the sweep, reports metrics back, handles checkpoints atomically, and
cooperates with preemption signals.

The SDK works in three modes: connected to `runqd` (full features),
file-only (HPC, no socket), or standalone (no runq at all — useful for
local debugging). Your script doesn't need to know which mode it's in.

See [sdk_reference.md](./sdk_reference.md) for the full API.
