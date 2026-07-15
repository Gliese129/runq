# runq

**If you are an AI agent, read the [runq skill](./.skills/runq/SKILL.md) first.**
**If you are a human, [docs/getting-started.md](./docs/getting-started.md) walks you through your first sweep in five minutes.**

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

One binary, no containers, no cluster admin. runq is not an ops tool — it
just helps you stop copy-pasting shell commands, stop digging through logs
to figure out which run used which hyperparameters, and stop wasting GPU
hours overnight.

## Why not just...

| | runq | tmux + nohup | SLURM / PBS |
|---|---|---|---|
| Setup | One binary, zero config | Already there | Need a cluster admin |
| GPU assignment | Automatic, no conflicts | Manual `CUDA_VISIBLE_DEVICES` | Automatic |
| Parameter sweep | One command or YAML | Hand-written for loop | sbatch arrays |
| "Which log had lr=0.01?" | `runq task show <id>` | Good luck with `log37_lr_1e-2--batch_32--20250514_0347.txt` | Job name conventions |
| Task fails at 3 AM | Auto-retry, next task starts | Dead. GPUs idle till morning | Depends on config |
| Lab has 3 users | Fair scheduling by GPU-hours | "Hey, are you using GPU 2?" | Built-in, but overkill |
| Your AI agent drives it | Ships with an agent skill + `--json` everywhere | Agent hand-rolls a scheduler every time | Agent scrapes qstat output |
| Works on HPC / SLURM | Yes (target mode) | Nope (login node thread limits) | It *is* SLURM |

runq fills the gap between "everyone just runs stuff manually" and "we need
a full cluster scheduler." It also works *on top of* HPC clusters (SLURM,
PBS, SGE): runq handles sweep expansion, param injection, and log
management; the cluster's own scheduler keeps scheduling. Same workflow on
a lab machine and on TSUBAME.

## Install

```bash
go install github.com/gliese129/runq/cmd/runq@latest
```

Each release ships four artifacts, all pinned to the same version tag:

| Artifact | What | When you need it |
|---|---|---|
| `runq-*` | CLI core (slim) | Always |
| `runq-dashboard-*` | CLI with the web dashboard embedded | You want a GUI |
| `runq-skills-*.zip` | Agent skill bundle | Your AI agent operates runq |
| `runq-sdk` on PyPI (`pip install runq-sdk`) | Python SDK | Metrics / checkpoints / early-stop in training code |

Daemon mode needs `nvidia-smi` on PATH. HPC mode needs only a working
cluster CLI (`sbatch`/`qsub`). Check any setup with `runq doctor`.

## Quickstart — local GPU machine

```bash
runq daemon start -d        # auto-detects your GPUs

cd ~/experiments/resnet50
runq init train.py          # scans argparse → project.yaml + job.yaml
runq project add .          # register the project

runq submit job.yaml --dry  # preview: exact task expansion, nothing submitted
runq submit job.yaml        # go — or skip YAML: runq sweep lr=1e-3,1e-2 bs=32,64
```

Then check on things:

```bash
runq ps                     # job table (ps <job_id> = that job's tasks)
runq gpu                    # per-GPU allocation
runq logs <task_id>         # tail + follow a task (--no-follow to just print)
runq best <job_id> --key val_loss     # best task by a logged metric (--max for accuracy-style)
```

Full walkthrough: [docs/getting-started.md](./docs/getting-started.md).

## Quickstart — HPC cluster (SLURM / PBS / SGE)

runq compiles your sweep, writes per-task workspaces, and delegates
scheduling to the cluster's native job manager. No resident daemon needed.

```bash
runq target add tsubame --template=tsubame --host=login.t4.gsic.titech.ac.jp --user=alice
runq target check tsubame   # renders every template with sample values — free
runq connect tsubame        # verify SSH + host key, install remote CLI, start forward

runq submit job.yaml --project-file project.yaml -t tsubame --dry
runq submit job.yaml --project-file project.yaml -t tsubame
runq target use tsubame     # or make -t the session default

runq ps                     # same commands as local — target-aware
runq status <job_id>
runq collect <job_id> --key loss
```

After `target add`, customise `submit_template` / `submit_id_regex` /
`kill_template` in `~/.runq/config.yaml` for your cluster (presets for
Slurm, PBS, SGE, TSUBAME, ABCI included), then re-run `target check`.
Details, per-task walltime/queue knobs, and login-node behavior:
[docs/hpc.md](./docs/hpc.md).

## Everyday commands

```bash
runq ps [job_id]             # jobs, or one job's tasks   (--json for machines)
runq status [job_id]         # daemon/queue summary, or refresh+show one job
runq logs <task_id>          # tail + follow; --no-follow to print and exit
runq kill <task_id|job_id>   # stop one task or a whole job
runq task retry <task_id>    # requeue a failed task
runq job pause|resume <id>   # stop/resume dispatching (running tasks continue)
runq job archive <id>        # hide from lists (reversible; data untouched)
runq clean --show            # preview cleanup; `runq clean` deletes IRREVERSIBLY
```

Every read command takes `--json`. Job ids look like `jb…`, task ids like
`tk…` — scripts should take them from `--json` output, not parse tables.
The complete command reference: [docs/cli.md](./docs/cli.md).

## Configuration in 20 lines

```yaml
# project.yaml — the parameter CATALOG: what CAN vary, and defaults
project_name: resnet50
working_dir: /home/user/experiments/resnet50
command_template: python train.py {{args}}    # {{args}} = --key=value for all params
environment:
  WANDB_PROJECT: resnet-experiments
params:
  - { name: lr, type: float, default: 0.001 }
  - { name: optimizer, type: str, default: adam, choices: [adam, sgd], strict: true }
defaults:
  gpus_per_task: 1
  max_retry: 3
```

```yaml
# job.yaml — one SELECTION over the catalog: what varies THIS time
project: resnet50
sweep:
  - method: grid                    # cartesian product within the block
    parameters: { lr: [0.001, 0.01, 0.1], optimizer: [adam, sgd] }
  - method: list                    # zip 1-to-1 — for values that belong together
    parameters: { batch_size: [32, 64], num_workers: [4, 8] }
# blocks cross-multiply: grid(3×2) × list(2) = 12 tasks
```

`project.yaml` is the source of truth — hand-edits are picked up on the
next use; CLI and GUI can never disagree. `.env` in `working_dir` is
sourced at task start and never stored by runq (tokens stay out of the DB,
logs, and UIs). Every field, placeholder, and priority rule:
[docs/configuration.md](./docs/configuration.md).

Copy-paste starting points live in [`examples/`](./examples/) — fully
annotated project.yaml / job.yaml, a minimal starter, and an
[`examples/hpc/`](./examples/hpc/) pair showing the model × benchmark
pattern with per-benchmark walltimes (`scope: scheduler` + zip pairing).

## Scheduling & reliability

Two strategies at daemon startup: **FIFO** or **fair-share** (users with
fewer consumed GPU-hours get priority), both with backfill and a 15-minute
anti-starvation reservation. GPU isolation via `CUDA_VISIBLE_DEVICES` —
each task sees only its assigned GPUs.

Built for the "submit before going home" workflow: auto-retry up to
`max_retry`, optional per-task timeout, daemon crash recovery from SQLite
(running processes are reclaimed, the queue is restored), GPU leak
detection after each task, and awareness of non-runq processes squatting
on GPUs.

## Documentation

| Doc | For |
|---|---|
| [examples/](./examples/) | Annotated, copy-paste config starting points (incl. an HPC pair) |
| [docs/getting-started.md](./docs/getting-started.md) | First sweep in 5 minutes (local machine) |
| [docs/configuration.md](./docs/configuration.md) | project.yaml / job.yaml full reference |
| [docs/hpc.md](./docs/hpc.md) | Cluster targets: setup, templates, login-node behavior |
| [docs/cli.md](./docs/cli.md) | Every command and flag |
| [docs/sdk_reference.md](./docs/sdk_reference.md) | Python SDK architecture |
| [docs/design_philosophy.md](./docs/design_philosophy.md) | Why runq behaves the way it does |
| [.skills/runq/](./.skills/runq/) | Installable operating skill for AI agents |

## Your AI agent already knows runq

runq ships an agent skill — a compact operating manual that teaches
Claude Code / Codex-style agents the correct workflow (dry-run first,
`--json` everywhere, never hand-assign GPUs) and the sweep semantics, so
they configure and drive runq correctly instead of reinventing a scheduler
in bash. Grab `runq-skills-*.zip` from Releases and drop it into your
repo's `.claude/skills/` (or `.codex/skills/`); a `runq skills install`
command is on the roadmap. Details in the [runq skill](./.skills/runq/SKILL.md).

## Python SDK

`pip install runq-sdk` integrates with your training script: typed params
auto-merged from the sweep, `runq.log_metric()` (feeds `runq best`),
atomic `safe_save()` checkpoints, cooperative preemption, early stopping.
Works in daemon mode, file-only mode (HPC), or with no runq at all.

```python
import runq

@runq.dataclass(auto_overwrite=True)
class Params:
    lr: float = 0.001

cfg = Params()                       # sweep params merged in
for step in runq.range(1000):        # preemption + early-stop aware
    runq.log_metric("loss", train_step(cfg))
    runq.safe_save("ckpt.pt", model.state_dict(), keep_last_n=3)
```

See [docs/sdk_reference.md](./docs/sdk_reference.md) and `sdk/python/examples/`.

## License

MIT
