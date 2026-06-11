# runq

A lightweight GPU job scheduler for research labs.

![runq overview poster](assets/poster-en.png)

If your lab's GPU workflow looks like this:

```bash
# The "I hope nobody takes GPU 3" workflow
export CUDA_VISIBLE_DEVICES=3
nohup python train.py --lr 0.001 --batch_size 32 > log1.txt &
# wait... check... done?
export CUDA_VISIBLE_DEVICES=3
nohup python train.py --lr 0.01 --batch_size 32 > log2.txt &
# repeat 4 more times, mass Ctrl+C when something goes wrong
# go to bed, wake up to 6 hours of idle GPUs
# next morning: which log was the lr=0.01 run again?
```

Or on an HPC cluster:

```bash
# The "I need 6 runs with different hyperparameters" workflow
for lr in 0.001 0.01 0.1; do
  for bs in 32 64; do
    sed "s/{{LR}}/$lr/g; s/{{BS}}/$bs/g" submit.sh.tmpl > submit_${lr}_${bs}.sh
    qsub submit_${lr}_${bs}.sh
  done
done
# 6 scripts generated, 6 qsub calls, hope you didn't typo the template
# later: which job ID was the lr=0.01, bs=64 run?
```

runq turns both into:

```bash
runq sweep lr=0.001,0.01,0.1 batch_size=32,64
# 6 tasks queued. GPUs assigned. Failures retried. You go to sleep.
# Each task has its own log, tagged with the exact params that produced it.
```

One binary, no containers, no cluster admin. runq is not an ops tool — it just helps you stop copy-pasting shell commands, stop digging through logs to figure out which run used which hyperparameters, and stop wasting GPU hours overnight.

## Why not just...

| | runq | tmux + nohup | SLURM / PBS |
|---|---|---|---|
| Setup | One binary, zero config | Already there | Need a cluster admin |
| GPU assignment | Automatic, no conflicts | Manual `CUDA_VISIBLE_DEVICES` | Automatic |
| Parameter sweep | One command or YAML | Hand-written for loop | sbatch arrays |
| "Which log had lr=0.01?" | `runq task show <id>` | Good luck with `log37_lr_1e-2--batch_32--20250514_0347.txt` | Job name conventions |
| Task fails at 3 AM | Auto-retry, next task starts | Dead. GPUs idle till morning | Depends on config |
| Lab has 3 users | Fair scheduling by GPU-hours | "Hey, are you using GPU 2?" | Built-in, but overkill |
| Disk fills up | Auto-pause + alert (planned) | Everything dies, good morning | Rarely an issue (dedicated storage) |
| Works on HPC / SLURM | Yes (template mode) | Nope (login node thread limits) | It *is* SLURM |

runq fills the gap between "everyone just runs stuff manually" and "we need a full cluster scheduler." If your lab moves between shared GPU machines and HPC jobs, this is meant to be the lightweight layer that keeps the workflow consistent.

It also works on HPC clusters (SLURM, PBS, SGE) — runq doesn't replace the cluster scheduler, it just handles sweep expansion, param injection, and log management on top of it. Same workflow, whether you're on a lab machine or submitting to TSUBAME.

## Install

```bash
go install github.com/gliese129/runq/cmd/runq@latest
```

This installs the CLI core. The dashboard UI and Python SDK are optional.

For the dashboard, download a `runq-dashboard-*` binary from GitHub Releases, or build it from source:

```bash
git clone https://github.com/gliese129/runq.git && cd runq
cd web/dashboard && yarn install --frozen-lockfile && yarn build
cd ../..
go build -tags dashboard -o runq ./cmd/runq
```

During dashboard UI development, run the backend with local assets:

```bash
runq dashboard --assets-dir internal/dashboard/dist
```

For the Python SDK:

```bash
pip install runq
```

Daemon mode requires `nvidia-smi` on PATH. Run `runq doctor` to check your setup.
HPC mode requires only a working cluster CLI (`sbatch`/`qsub`).

## Quickstart — Daemon (Local GPU Machine)

```bash
# 1. Start the daemon (auto-detects your GPUs)
runq daemon start

# 2. Set up your experiment directory
cd ~/experiments/resnet50
runq init train.py          # scans argparse, generates project.yaml + job.yaml

# 3. Submit
runq submit .               # or just: runq sweep lr=0.001,0.01,0.1 batch_size=32,64

# 4. Check on things
runq ps                     # task status
runq gpu                    # GPU allocation
runq logs <task_id>         # tail output
```

That's it. runq expands the parameter sweep, queues the tasks, assigns GPUs, and retries failures.

## Quickstart — HPC (Slurm / PBS / SGE)

On a shared cluster, runq compiles your sweep, writes per-task workspaces, and delegates scheduling to the cluster's native job manager. No resident daemon needed.

```bash
# 1. Generate config (edit it for your cluster, then validate)
runq hpc init --scheduler slurm     # also: pbs | sge | tsubame | abci
runq hpc config check               # renders every template with sample values

# 2. Write project.yaml + job.yaml (same format as daemon mode)
#    See examples/ for templates

# 3. Preview, then submit
runq hpc submit job.yaml --project-file project.yaml --dry-run
runq hpc submit job.yaml --project-file project.yaml

# 4. Monitor
runq hpc ls                          # list jobs
runq hpc status <job_id>             # refresh from disk + show tasks
runq hpc best <job_id> --key loss    # best task by metric
runq hpc collect <job_id> --key loss # all tasks ranked by metric
```

After `runq hpc init`, edit `~/.runq/config.yaml` to match your cluster — the `submit_template`, `submit_id_regex`, and `kill_template` fields must be correct for your scheduler (also editable in the dashboard Settings page, with presets, placeholder hints and a built-in checker). Presets for Slurm, PBS, SGE, TSUBAME and ABCI are provided as starting points.

Per-task scheduler knobs (walltime, queue) can live in the sweep and be referenced from `submit_template` as `{{param.h_rt}}`, `{{param.node_kind}}` — one job can carry per-benchmark time limits. Declare them with `scope: scheduler` in project.yaml so they're consumed by the submit command, not your training command (see Configuration).

## Usage

### Quick one-off run

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

Sweep blocks combine via cross-product. This produces 6 × 3 = 18 tasks.

```bash
runq submit .                # reads ./job.yaml
runq submit --dry-run .      # preview tasks without submitting
```

### Job control

```bash
runq job pause <job_id>      # pause scheduling (running tasks continue)
runq job resume <job_id>
runq job kill <job_id>       # kill all tasks in a job
runq kill <task_id>          # kill one task
runq task retry <task_id>    # retry a failed task
```

### Monitoring

```bash
runq ps                      # running + pending
runq ps -a                   # include completed
runq ps --status failed      # filter
runq gpu                     # per-GPU allocation
runq logs <task_id>          # tail output
runq task show <task_id>     # params, status, timing, log path
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

Command templates support `{{args}}` (auto-generates `--key=value` for all parameters) and `{{param_name}}` (inserts a specific parameter). You can mix both: `python train.py --lr {{lr}} {{args}}`.

More project-level fields:

- `params:` — the parameter **catalog** (`type` / `default` / `choices` / `include` / `strict` / `scope`). The dashboard builds its submit form from this; a job.yaml is just one selection over it (`grid` = cross product, `list` = zip).
  - `strict: true` upgrades `choices` from suggestions to a contract: any submitted value outside the list fails at submit time (CLI and GUI alike).
  - `scope: scheduler` declares a param consumed by the HPC `submit_template` (`{{param.h_rt}}`) instead of the command — it is exempt from command-template consumption and never injected into `{{args}}`. Recommended pattern for queue-like knobs:

    ```yaml
    params:
      - { name: node_kind, type: str, scope: scheduler, default: node_q, choices: [node_q, node_h, node_f], strict: true }
    ```
- `setup_command:` — runs once per submit, before anything is persisted (e.g. `hf download {{model}}`). May reference fixed params only; failure aborts cleanly.
- `environment:` — injected into every task AND prefixed onto the HPC submit command, so `$TSUBAME_GROUP`-style references in `submit_template` resolve from project config.
- `job_name:` — template for the per-task scheduler job name, exposed to `submit_template` as `{{name}}` (params + `{{project}}` `{{job_id}}` `{{task_id}}`; default `rq-{{task_id}}`). Always sanitized — scheduler-safe charset, never digit-first. job.yaml `name:` overrides per submission.
- `.env` in `working_dir` is sourced at task start automatically (override with `env_file:`). runq never stores its values — tokens stay out of the DB, logs and UIs. Explicit `environment:` always wins.
- Job `note` supports placeholders: params, `{{project}}` `{{user}}` `{{date}}` `{{time}}` `{{sweep}}`, and `{{version}}` — re-running the same named config auto-numbers it (`foo`, `foo-v2`, `foo-v3`), with timestamp differences ignored when finding the family.

### Python environments

runq auto-detects uv, venv, and conda environments in your project directory and activates them before running tasks. Detection happens during `runq init` and can be overridden in project.yaml.

### Config priority

CLI flag > job.yaml override > project.yaml default > built-in default.

`project.yaml` is the source of truth: hand-edits are picked up automatically the next time the project is selected or submitted (the DB copy is just a cache and re-syncs on read) — CLI and GUI can never disagree about a project.

## Scheduling

Two strategies, selectable at daemon startup:

**FIFO** — first submitted, first scheduled. Simple.

**Fair-share** — users who have consumed fewer GPU-hours get priority. If one person submits a 50-task sweep, everyone else doesn't have to wait till next week.

Both include backfill (small tasks run in gaps while big tasks wait for GPUs) and reservation (tasks waiting longer than 15 minutes get priority, preventing starvation).

GPU isolation uses `CUDA_VISIBLE_DEVICES`. Each task sees only its assigned GPUs.

## Reliability

runq is designed for the "submit before going home" workflow:

- **Auto-retry** — failed tasks retry up to `max_retry` times.
- **Timeout** — optional per-task timeout kills runaway processes.
- **Daemon crash recovery** — restarts reclaim still-running processes and restore the full queue from SQLite. Nothing is lost.
- **GPU leak detection** — checks for residual processes after each task exits.
- **External GPU awareness** — detects non-runq processes on GPUs and avoids those slots.


<details>
<summary><strong>CLI Reference — Daemon</strong></summary>

| Command | Description |
|---|---|
| `runq init [script.py]` | Scaffold project.yaml + job.yaml |
| `runq submit [path] [--note "..."]` | Submit a job from YAML |
| `runq sweep [--note "..."]` | Submit a sweep from CLI args |
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
| `runq clean --older-than <dur>` | Remove finished tasks, workspaces, and orphan jobs |

</details>

<details>
<summary><strong>CLI Reference — HPC</strong></summary>

| Command | Description |
|---|---|
| `runq hpc init [--scheduler slurm\|pbs\|sge]` | Generate `~/.runq/config.yaml` template |
| `runq hpc submit <job.yaml> [--note "..."]` | Compile sweep + submit each task to the cluster |
| `runq hpc status <job_id>` | Refresh from disk and show task status |
| `runq hpc kill <job_id\|task_id>` | Cancel via kill_template |
| `runq hpc ls` | List HPC jobs (DB state) |
| `runq hpc best <job_id> --key <metric>` | Show the task with the best metric value |
| `runq hpc collect <job_id> --key <metric>` | Per-task params + best metric, ranked |
| `runq hpc rm <job_id>` | Remove a completed job from DB |
| `runq hpc clean --older-than <dur>` | Delete old tasks, workspaces, and empty jobs |

</details>

<details>
<summary><strong>File Locations</strong></summary>

| Path | Description |
|---|---|
| `~/.runq/config.yaml` | Global config (`data_path`) + HPC config (`hpc:` section) |
| `~/.runq/runq.db` | SQLite database (daemon) |
| `~/.runq/runq.sock` | Unix domain socket (daemon) |
| `~/.runq/daemon.pid` | PID file (daemon) |
| `<root>/<job_id>/<task_id>/` | Per-task workspace |
| `<task_dir>/params.json` | Sweep-expanded parameters |
| `<task_dir>/metrics.jsonl` | Training metrics |
| `<task_dir>/checkpoints/` | Checkpoint directory |
| `<task_dir>/run.sh` | Generated wrapper script (HPC only) |
| `<task_dir>/status.json` | Self-reported task status (HPC only) |

By default `<root>` is `<working_dir>/.runq/`. If `data_path` is set in `config.yaml`, physical storage moves to `<data_path>/<project>/` and `.runq` becomes a convenience symlink.

</details>

## Python SDK

runq ships an optional Python SDK (`pip install runq`) that integrates with your training script. The SDK handles parameter injection, checkpoint safety, metrics logging, and cooperative preemption — all without changing your training loop structure.

```python
import runq

ctx = runq.context()

# Typed params — auto-merged from sweep parameters
@runq.dataclass(auto_overwrite=True)
class Params:
    lr: float = 0.001
    batch_size: int = 32

cfg = Params()  # cfg.lr may be overridden by the sweep

# Training loop with preemption + early stop
for step in runq.range(1000):
    loss = train(model, cfg)
    runq.log_metric("loss", loss)
    runq.report({"val_loss": evaluate(model)})  # early-stop check
    runq.safe_save("ckpt.pt", model.state_dict(), keep_last_n=3)

# Resume
ckpt = runq.latest_checkpoint()  # or runq.best_checkpoint()
```

Key features:

- **`runq.range()` / `runq.loop()`** — drop-in iterators with SIGTERM preemption and early-stop. `range()` for numeric loops, `loop()` for arbitrary iterables (dataloaders).
- **`@runq.dataclass`** — typed parameter class with `to_json`/`from_json`/`to_yaml`/`from_yaml`. `auto_overwrite=True` merges sweep params at instantiation.
- **`runq.safe_save()`** — atomic checkpoint writes (tmp + fsync + rename). Catches ENOSPC mid-write, triggers freeze flow in daemon mode. Manifest-scoped cleanup via `keep_last_n` / `keep_best`.
- **`runq.seed`** — deterministic per-task seed derived from task ID.
- **`runq.report()`** — early-stop evaluation with pluggable policies (`patience`, `threshold`, `convergence`).
- **Tri-mode** — works with daemon (Unix socket), without daemon (file-only), or in manual mode (no runq infrastructure at all).

The SDK is in `sdk/python/`. Install from the repo:

```bash
cd sdk/python && pip install -e .
```

## License

MIT
