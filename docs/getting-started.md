# Getting started — your first sweep in 5 minutes

This walkthrough assumes a shared lab machine with NVIDIA GPUs. For HPC
clusters (SLURM / PBS / SGE) do steps 1–3 here, then switch to
[hpc.md](./hpc.md).

## 1. Install and check

```bash
curl -fsSL https://raw.githubusercontent.com/Gliese129/runq/main/install.sh | sh
runq doctor
```

The installer supports Linux and macOS on amd64/arm64. It asks whether to
install the embedded UI build, verifies the downloaded checksums, installs
`runq` (plus Linux-only `runqd`), and starts `runq` in the background.
`doctor` then tells you exactly what's missing and how to fix it.

The one-liner remains scriptable: set `RUNQ_WITH_UI=0` or `1`,
`RUNQ_START_DAEMON=0` to skip startup, `RUNQ_INSTALL_DIR` for a custom
destination, and `RUNQ_VERSION=v…` to pin a release.

```bash
curl -fsSL https://raw.githubusercontent.com/Gliese129/runq/main/install.sh \
  | RUNQ_WITH_UI=1 RUNQ_VERSION=v0.5.0rc2 sh
```

## 2. Start the daemon

```bash
runq daemon start -d
```

The installer already runs this command; use it after a manual install or
after stopping the client daemon. `runq` owns the cross-platform client and
dashboard process. On a Linux local target it finds the sibling `runqd` and
starts that headless scheduler/executor as needed. `runqd` is intentionally
separate so the client can also run on macOS and drive Linux/HPC targets.
`runq status` shows the queue summary any time.

## 3. Turn your script into a project

```bash
cd ~/experiments/resnet50
runq init train.py
```

`init` scans `train.py`'s argparse arguments and generates two files:

- **project.yaml** — the parameter *catalog*: every argument it found, with
  types and defaults, plus `command_template`, `working_dir`, and GPU
  defaults. This describes what *can* vary.
- **job.yaml** — one *selection* over that catalog: which values to sweep
  *this time*.

Open project.yaml and sanity-check three things: `working_dir` (must
exist), `command_template` (how your script is invoked; `{{args}}` expands
to `--key=value` for every parameter), and `defaults.gpus_per_task`.

Your script doesn't use argparse? Copy the annotated examples in
[configuration.md](./configuration.md) and fill in the params by hand.

Then register:

```bash
runq project add .
```

## 4. Describe the sweep

Edit job.yaml:

```yaml
project: resnet50
note: "first sweep"           # human label; shows up in runq ps
sweep:
  - method: grid              # cartesian product
    parameters:
      lr: [0.001, 0.01, 0.1]
      batch_size: [32, 64]
```

Or skip the file entirely for quick things:

```bash
runq sweep lr=0.001,0.01,0.1 batch_size=32,64      # same 6 tasks, no YAML
runq run resnet50 --gpus 2 -- --lr 0.01            # one-off single task
```

## 5. Preview, then submit

```bash
runq submit job.yaml --dry
```

`--dry` prints the exact expanded task list — `lr(3) × batch_size(2) = 6
tasks` — and submits nothing. It costs nothing; make it a habit. When the
expansion looks right:

```bash
runq submit job.yaml
```

runq queues 6 tasks, assigns free GPUs as they become available, isolates
each task to its GPUs via `CUDA_VISIBLE_DEVICES`, retries failures up to
`max_retry`, and gives each task its own log tagged with its exact params.

## 6. Watch, and collect results

```bash
runq ps                      # job table: id, status, done/total, note
runq ps <job_id>             # that job's tasks with their params
runq gpu                     # who is on which GPU
runq logs <task_id>          # tail + follow (Ctrl-C to stop; --no-follow to just print)
```

If your script logs metrics (via the Python SDK's `runq.log_metric()`, or
by writing `metrics.jsonl` in the task workspace):

```bash
runq best <job_id> --key val_loss          # single best task + its params
runq best <job_id> --key accuracy --max    # higher-is-better metrics need --max
runq collect <job_id> --key val_loss       # every task, ranked
```

## 7. When something goes wrong

```bash
runq task show <task_id>     # params, status, retry count, log path
runq task retry <task_id>    # requeue just that one
runq kill <task_id>          # stop one task
runq kill <job_id>           # stop the whole job
runq job pause <job_id>      # stop dispatching; running tasks continue
```

runq's own operation log (every submit/kill with the rendered command) is
at `~/.runq/logs/runq.log`. And when a run is truly over:

```bash
runq clean --show            # preview what would be deleted
runq clean --older-than 720h # delete finished tasks + artifacts (IRREVERSIBLE)
```

## Where things live

| Path | What |
|---|---|
| `~/.runq/config.yaml` | Global config + HPC targets |
| `~/.runq/runq.db` | Queue state (SQLite; daemon crash recovery reads this) |
| `<working_dir>/.runq/<note>-<job_id>/<task_id>/` | Per-task workspace: log, params.json, metrics.jsonl, results.jsonl, events.jsonl, checkpoints/ |

The job directory starts with your `note`, so `ls .runq/` reads like an
experiment log. Don't parse these paths in scripts — use `--json` output.

Next steps: [configuration.md](./configuration.md) for the full YAML
reference · [hpc.md](./hpc.md) to run the same workflow on a cluster ·
[runq skill](../.skills/runq/SKILL.md) if an AI agent will drive runq for you.
