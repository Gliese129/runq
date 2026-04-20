# runq

A lightweight, single-machine GPU job scheduler for research labs.

runq manages GPU allocation, parameter sweep expansion, and task lifecycle with minimal configuration. It runs as a local daemon and is controlled entirely via CLI.

## Data Model

```
Project → Job → Task
```

- **Project** — a registered experiment type: command template, GPU defaults, resume config.
- **Job** — a submitted sweep that expands into multiple tasks via grid/list parameter combinations.
- **Task** — the smallest schedulable unit: one command on N GPUs.

## Quick Start

```bash
# Build
go build -o runq ./cmd/runq

# Start the daemon
runq daemon start

# Register a project
runq project add .              # reads ./project.yaml

# Submit a sweep job
runq submit .                   # reads ./job.yaml

# Monitor
runq ps                         # running + pending tasks
runq gpu                        # GPU allocation
runq status                     # queue summary
runq logs <task_id>             # tail task output

# Kill
runq kill <task_id>             # kill one task
runq kill <job_id>              # kill all tasks in a job
```

## Configuration

### project.yaml

Defines an experiment type. Register with `runq project add .`.

```yaml
project_name: resnet50
working_dir: /home/user/projects/resnet50
command_template: python train.py {{args}}

environment:
  WANDB_PROJECT: resnet-experiments

defaults:
  gpus_per_task: 1
  max_retry: 3          # 0 = unlimited

resume:
  enabled: true
  extra_args: --resume --ckpt latest
```

### job.yaml

Defines a parameter sweep. Submit with `runq submit .`.

```yaml
project: resnet50

sweep:
  # Grid: cartesian product within the block
  - method: grid
    parameters:
      lr: [0.001, 0.01, 0.1]
      optimizer: [adam, sgd]

  # List: zip parameters 1-to-1 (must be same length)
  - method: list
    parameters:
      batch_size: [32, 64, 128]
      num_workers: [4, 8, 16]
```

Blocks are combined via cross-product. The example above produces 6 × 3 = 18 tasks.

### Command Templates

Templates in `command_template` support two modes:

- `{{args}}` — auto-generates `--key=value` for all parameters, sorted by key.
- `{{param_name}}` — inserts a specific parameter by name. Unconsumed parameters go to `{{args}}` if present.

Mixed mode is supported: `python train.py --lr {{lr}} {{args}}`.

### Config Priority

CLI flag > YAML field > built-in default. Per-job overrides in `job.yaml` take precedence over project defaults.

## Scheduling

runq uses a FIFO queue with three enhancements:

- **Reservation** — large multi-GPU tasks reserve a slot so they aren't starved by small tasks.
- **Aging** — tasks waiting longer than 1 hour get priority boost.
- **Backfill** — while a large task waits for enough GPUs, smaller tasks that fit in the remaining slots can run.

GPU isolation is soft (via `CUDA_VISIBLE_DEVICES`). Each task gets its assigned GPU indices and cannot see other GPUs.

## Architecture

```
┌────────┐     unix socket     ┌─────────────────────────┐
│  CLI   │ ◄──────────────────► │       Daemon            │
│ (cobra)│                      │                         │
└────────┘                      │  Gin Router (API)       │
                                │  ├── Project Registry   │
                                │  ├── Queue              │
                                │  ├── Scheduler          │
                                │  │   ├── GPUPool        │
                                │  │   └── Executor       │
                                │  └── Store (SQLite)     │
                                └─────────────────────────┘
```

The daemon exposes a REST API over a unix domain socket. The CLI communicates with the daemon via HTTP over this socket. This same API can later serve a web UI.

### Internal Packages

| Package | Role |
|---|---|
| `cli` | Cobra commands, Client wrapper, table output |
| `api` | Gin handlers, Server lifecycle, unix socket Client |
| `scheduler` | Queue (FIFO + aging + backfill), GPUPool, scheduling loop |
| `executor` | Process management (`os/exec`), log redirection, PID reclaim |
| `job` | Sweep expansion (grid/list), command template rendering |
| `project` | Project config struct, SQLite-backed Registry |
| `store` | SQLite connection (WAL mode), schema migrations |
| `gpu` | `nvidia-smi` detection and parsing |
| `utils` | Process start time reader, atomic file writes |

### Tech Stack

- **CLI**: Cobra
- **API**: Gin
- **Storage**: modernc.org/sqlite (pure Go, no CGO)
- **Config**: gopkg.in/yaml.v3
- **Logging**: log/slog (stdlib)

## CLI Reference

### Shortcuts

| Command | Description |
|---|---|
| `runq submit <path>` | Submit a job from YAML |
| `runq run <project> -- <args>` | Quick single task without YAML |
| `runq ps` | List running + pending tasks |
| `runq logs <task_id>` | Tail task output |
| `runq kill <id>` | Kill a task or job |
| `runq gpu` | GPU allocation status |
| `runq status` | Daemon/queue summary |

### Resource Management

| Command | Description |
|---|---|
| `runq project add .` | Register project from YAML |
| `runq project ls` | List projects |
| `runq project show <name>` | Show project details |
| `runq project edit <name>` | Edit in $EDITOR |
| `runq project rm <name>` | Remove project |

### Job & Task

| Command | Description |
|---|---|
| `runq job ls` | List jobs with status breakdown |
| `runq job kill <job_id>` | Kill all tasks in a job |
| `runq task show <task_id>` | Show task details |

### Daemon

| Command | Description |
|---|---|
| `runq daemon start` | Start scheduler daemon |
| `runq daemon stop` | Stop daemon (SIGTERM) |
| `runq daemon restart` | Stop + start |

### Common Flags

- `runq ps -a` — include completed tasks
- `runq ps --status failed` — filter by status
- `runq ps --job <id>` — filter by job
- `runq ps -o json` — JSON output
- `runq ps --no-header` — suppress table header
- `runq submit --dry-run` — preview expanded tasks without submitting

## File Locations

| Path | Description |
|---|---|
| `~/.runq/runq.sock` | Unix domain socket |
| `~/.runq/daemon.pid` | PID file |
| `~/.runq/runq.db` | SQLite database |
| `logs/<task_id>.log` | Task output (relative to project working_dir) |

## License

MIT
