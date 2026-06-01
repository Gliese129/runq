# AI_README — runq Setup Guide for AI Agents

> You are helping a user set up runq, a GPU job scheduler for research labs.
> For background, see README.md. For example configs, see examples/.

## Step 1: Build

```bash
go build -o runq ./cmd/runq
```

Put the binary on PATH. Verify with `runq --help`.

## Step 2: Align with the User

Before touching any config, ask the user these questions:

1. **Environment**: Local GPU machine, or HPC cluster (Slurm/PBS/SGE)?
   → Determines daemon mode vs `runq hpc` mode.

2. **Training command**: What does their training invocation look like?
   (e.g. `python train.py --lr 0.01 --batch_size 32`)
   → Drives `command_template` in project.yaml.

3. **Sweep parameters**: Which hyperparameters to sweep, what values, grid or list?
   → Drives job.yaml.

4. **GPU count**: How many GPUs per task?

5. **Storage** (HPC only): Default location (`<project>/.runq/`) or a custom
   path (e.g. scratch disk)?
   → Drives `data_path` in `~/.runq/config.yaml`.

6. **Cluster details** (HPC only): Which scheduler? Any special flags for
   submit (partition, account, QOS, time limit)?
   → Drives `hpc:` section in `~/.runq/config.yaml`.

## Step 3: Configure & Submit

### Daemon mode

1. `runq doctor` — verify GPU environment
2. `runq daemon start --detach`
3. Write `project.yaml` and `job.yaml` (see `examples/` for templates)
4. `runq project add .`
5. `runq submit --dry-run .` → confirm with user → `runq submit .`

### HPC mode

1. `runq hpc init --scheduler <slurm|pbs|sge>`
2. Edit `~/.runq/config.yaml` — must match the user's cluster
   (see `internal/hpcconfig/` for preset templates and field docs)
3. Write `project.yaml` and `job.yaml` (same format as daemon)
4. `runq hpc submit job.yaml --project-file project.yaml`

## Key Files to Read

| When you need to... | Read |
|---|---|
| Understand project.yaml schema | `examples/project.yaml` |
| Understand job.yaml / sweep syntax | `examples/job.yaml`, `examples/job_simple.yaml` |
| Understand HPC config fields | `internal/hpcconfig/hpcconfig.go` (presets + validation) |
| Understand global config (data_path) | `internal/config/config.go` |
| See all CLI commands | `internal/cli/root.go`, `internal/cli/hpc.go` |
| Understand task directory layout | `internal/workspace/workspace.go` |

## Pitfalls

- Daemon mode needs `nvidia-smi` on PATH and the daemon running.
- HPC `runq hpc init` writes a **template** — user must customize it.
- On HPC, login node may differ from compute node — use `--no-preflight` if pip/import checks fail at submit.
- `working_dir` in project.yaml must exist at submit time.
