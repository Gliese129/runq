# runq

A lightweight GPU job scheduler for research labs. One binary, no containers, no cloud — just `runq submit` and your GPUs stay busy.

runq manages GPU allocation, parameter sweeps, task retry, and fair scheduling across users on a single multi-GPU machine. It runs as a local daemon controlled via CLI.

## Install

```bash
go install github.com/gliese129/runq/cmd/runq@latest
# or build from source
git clone https://github.com/gliese129/runq.git && cd runq
go build -o runq ./cmd/runq
```

Requires `nvidia-smi` on PATH. Run `runq doctor` to verify your environment.

## 5-Minute Quickstart

```bash
# 1. Start the daemon (detects GPUs automatically)
runq daemon start

# 2. Scaffold a project config in your experiment directory
cd ~/experiments/resnet50
runq init train.py          # generates project.yaml + job.yaml

# 3. Edit the generated job.yaml to define your sweep
#    (or just use the defaults for a single run)

# 4. Submit
runq submit .

# 5. Watch
runq ps                     # task status
runq gpu                    # GPU allocation
runq logs <task_id>         # live output
```

That's it. runq expands your parameter sweep, queues the tasks, assigns GPUs, and handles retries.

## Core Concepts

```
Project  →  Job  →  Task
```

A **project** is a registered experiment type (command template, GPU defaults, retry policy). A **job** is a submitted sweep that expands into **tasks** — the actual GPU processes.

## Usage Examples

### Quick one-off run (no YAML)

```bash
runq run resnet50 --gpus 2 -- --lr 0.01 --epochs 100
```

### Parameter sweep from CLI

```bash
runq sweep --project resnet50 lr=0.001,0.01,0.1 batch_size=32,64
# Creates 6 tasks (cartesian product), submits as a job
```

### Parameter sweep from YAML

```yaml
# job.yaml
project: resnet50

sweep:
  - method: grid
    parameters:
      lr: [0.001, 0.01, 0.1]
      optimizer: [adam, sgd]

  - method: list
    parameters:
      batch_size: [32, 64, 128]
      num_workers: [4, 8, 16]
```

Sweep blocks combine via cross-product. This example produces 6 × 3 = 18 tasks.

```bash
runq submit .                # reads ./job.yaml
runq submit --dry-run .      # preview tasks without submitting
```

### Job control

```bash
runq job pause <job_id>      # pause scheduling (running tasks continue)
runq job resume <job_id>     # resume
runq job kill <job_id>       # kill all tasks
runq kill <task_id>          # kill one task
runq task retry <task_id>    # retry a failed task
```

### Monitoring

```bash
runq ps                      # running + pending
runq ps -a                   # include completed
runq ps --status failed      # filter by status
runq ps --job <job_id>       # filter by job
runq status                  # daemon summary
runq gpu                     # per-GPU allocation
runq logs <task_id>          # tail output
```

## Configuration

### project.yaml

```yaml
project_name: resnet50
working_dir: /home/user/experiments/resnet50
command_template: python train.py {{args}}

environment:
  WANDB_PROJECT: resnet-experiments

defaults:
  gpus_per_task: 1
  max_retry: 3

resume:
  enabled: true
  extra_args: --resume --ckpt latest
```

**Command templates** support `{{args}}` (auto-generates `--key=value` for all parameters) and `{{param_name}}` (inserts a specific parameter). Mixed mode works: `python train.py --lr {{lr}} {{args}}`.

### Python environment

runq auto-detects uv, venv, and conda environments in your project directory and activates them before running tasks. Detection runs during `runq init` and can be overridden in project.yaml.

### Config priority

CLI flag > job.yaml override > project.yaml default > built-in default.

## Scheduling

runq supports two scheduling strategies, selectable at daemon startup:

**FIFO** (default for simplicity) — first submitted, first scheduled.

**Fair-share** (default in production) — users who have consumed fewer GPU-hours get priority. Scored on three dimensions: pending demand, running occupation, and historical usage in a 24-hour sliding window.

Both strategies include:

- **Backfill** — while a large task waits for GPUs, smaller tasks that fit in remaining slots can run.
- **Reservation** — tasks waiting longer than 15 minutes get exclusive reservation, preventing starvation.

GPU isolation uses `CUDA_VISIBLE_DEVICES`. Each task sees only its assigned GPUs.

## Reliability

runq is designed to survive failures without losing work:

- **Task retry** — failed tasks are automatically retried up to `max_retry` times.
- **Task timeout** — optional per-task timeout auto-kills runaway processes.
- **Daemon restart recovery** — if the daemon crashes, it reclaims still-running processes and restores the full queue from SQLite.
- **GPU leak detection** — after each task exits, runq checks for residual processes still occupying GPUs.
- **External GPU awareness** — periodically scans for non-runq processes using GPUs and blocks those slots from scheduling.

## Architecture

```
CLI (cobra)  ──unix socket──►  Daemon
                                ├── API (gin)
                                ├── Scheduler
                                │   ├── Queue
                                │   ├── Prioritizer (FIFO / Fair-share)
                                │   └── GPU Pool
                                ├── Executor (os/exec)
                                └── Store (SQLite)
```

The daemon exposes a REST API over a unix domain socket. All state is persisted to SQLite; the in-memory queue is rebuilt from DB on restart.

## CLI Reference

| Command | Description |
|---|---|
| `runq init [script.py]` | Scaffold project.yaml + job.yaml |
| `runq submit [path]` | Submit a job from YAML |
| `runq sweep` | Submit a sweep from CLI args |
| `runq run <project> -- <args>` | Quick single-task run |
| `runq ps` | List tasks |
| `runq gpu` | GPU allocation |
| `runq status` | Daemon/queue summary |
| `runq logs <task_id>` | Tail task output |
| `runq kill <id>` | Kill task or job |
| `runq job ls/show/pause/resume/kill/rm` | Job management |
| `runq task show/retry` | Task management |
| `runq project add/ls/show/edit/rm` | Project management |
| `runq daemon start/stop/restart` | Daemon lifecycle |
| `runq doctor` | Environment check |
| `runq clean` | Remove finished tasks and orphan jobs |

## File Locations

| Path | Description |
|---|---|
| `~/.runq/runq.db` | SQLite database (all state) |
| `~/.runq/runq.sock` | Unix domain socket |
| `~/.runq/daemon.pid` | PID file |
| `<working_dir>/logs/<task_id>.log` | Task output |

## License

MIT
