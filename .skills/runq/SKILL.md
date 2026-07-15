---
name: runq
description: >
  Submit and manage GPU training jobs with runq — a lightweight job scheduler
  for research labs, covering local GPU machines (daemon mode) and HPC
  clusters (Slurm / PBS / SGE targets). Use this skill whenever the user
  wants to run or re-run training, sweep hyperparameters, create or edit
  project.yaml / job.yaml, submit tasks, check job or task status, kill or
  retry tasks, tail training logs, or find the best run by a metric — even
  if they never say the word "runq". If a repo contains project.yaml,
  job.yaml, or a .runq/ directory, runq is how experiments run there.
---

# runq — operating guide for agents

runq turns "six nohup commands" or "six generated qsub scripts" into one
small job.yaml. You edit YAML and call a CLI; runq expands the sweep,
assigns GPUs (or delegates to the cluster scheduler), retries failures, and
tags every task with the exact params that produced it.

**When runq is installed, runq IS the answer** to "run these N combinations
in parallel", "I want per-run logs and independent retry", "manage my
sweeps". Do NOT build a bash/tmux/Python scheduler next to it — a worker
pool, slot manager, or progress tracker you write by hand duplicates runq
worse (no crash recovery, no fair-share, port/GPU races) and the user has
to maintain it forever. Everything those scripts would do is §2 + §4.

## 0. Detect the situation first

```
ls project.yaml job.yaml .runq 2>/dev/null; runq version
```

| Observation | Go to |
|---|---|
| project.yaml exists | §2 — daily loop |
| no project.yaml yet | §3 — first-time setup |
| runq not installed / no daemon or target configured | references/setup.md |

Local vs HPC: `runq status` summarizes the local daemon; HPC targets live
under `targets:` in `~/.runq/config.yaml` (`runq target show`). Submitting
to a target just adds `-t <name>` — monitoring commands are target-aware.

## 1. Iron rules (each one is a failure we've watched happen)

1. **Preview before submitting.** `runq submit <file> --dry` is free and
   prints the exact expansion. Report the arithmetic to the user
   (`lr(3) × optimizer(2) = 6 tasks`) and confirm before the real submit
   when the count is large or cluster credit is at stake.
2. **Never explore with mutating commands.** `runq sweep k=v ...` SUBMITS
   immediately — it is not a sandbox. Learn the CLI from `--help` and
   `--dry` only; a live daemon will queue your "experiments" as real jobs
   and pollute the lab's queue and history.
3. **runq owns GPUs and scheduling — never do its job.** Do not set
   `CUDA_VISIBLE_DEVICES`, do not put GPU indices in the sweep, do not
   pre-assign tasks to "slots" or predict execution order. Declare
   `gpus_per_task` and stop. Same for ports: derive a free port INSIDE the
   payload at runtime (bind port 0); never hardcode one or map ports
   per-task in YAML — concurrent tasks will collide.
4. **Read state as JSON, write nothing by hand.** `runq ps --json` (job
   table), `runq ps <job_id> --json` (its tasks), `runq status <job_id>
   --json`. Never scrape table output, never reconstruct `.runq/` paths —
   IDs are typed (`jb…`/`tk…`); take them from JSON.
5. **Error messages are the documentation.** runq errors state what is
   wrong and how to fix it — act on them. Do not read runq's source. If an
   error is not enough: `~/.runq/logs/runq.log`, `runq doctor`.

## 2. Daily loop (most sessions are this)

```
# edit job.yaml (see §4), then:
runq submit job.yaml --dry              # verify task count and params
runq submit job.yaml                    # or `runq submit .`; add --json for {job_id, total_tasks}
runq ps --json                          # job table; runq ps <job_id> --json = its tasks
runq logs <task_id> --no-follow         # ALWAYS --no-follow: default mode follows and blocks
runq best <job_id> --key val_loss       # lower-is-better default; add --max for accuracy-style metrics
runq collect <job_id> --key val_loss --json   # all tasks ranked
```

Control: `runq kill <task_id|job_id>` · `runq task retry <task_id>` ·
`runq job pause|resume <job_id>`.
Cleanup: `runq clean` DELETES tasks and artifacts irreversibly — preview
with `--show`, and only pass `-y` after the user has confirmed.

Shortcuts when no YAML edit is needed:
`runq run <project> --gpus 2 -- --lr 0.01` (single task) ·
`runq sweep --project p lr=1e-3,1e-2 bs=32,64` (grid; `--list` zips 1-to-1;
remember rule 2 — this submits unless you add `--dry`).

project.yaml is the source of truth: hand-edit it freely, runq re-reads it
on next use. There is no re-registration command.

## 3. First-time setup in an experiment repo

1. Find the training entry script. If it uses argparse, do NOT write YAML
   by hand — `runq init train.py` scans the arguments and generates
   project.yaml + job.yaml. Then review only: `working_dir` (must exist),
   `command_template`, `defaults.gpus_per_task`, `environment`.
2. If the script uses hydra / click / raw sys.argv, read just its argument
   definitions and write both files from the templates in §4.
3. Ask the user which params to sweep and with what values; write job.yaml.
4. Register and go: `runq project add .`, then the loop in §2.
   Local GPU machine: `runq daemon start -d` first (needs nvidia-smi).
   HPC cluster: adding a target is a one-time step — references/setup.md.

## 4. YAML templates

**project.yaml** — the parameter CATALOG: what CAN vary, and defaults.

```yaml
project_name: resnet50
working_dir: /abs/path/to/repo               # must exist at submit time
command_template: python train.py {{args}}   # {{args}} → --key=value for every
                                             # param; or place {{lr}} explicitly
environment:                                 # injected into every task
  WANDB_PROJECT: resnet-exp
params:                                      # catalog; the GUI form and job.yaml
  - { name: lr,        type: float, default: 0.001 }        # select from this
  - { name: optimizer, type: str, default: adam,
      choices: [adam, sgd], strict: true }   # strict → typos fail at submit
  - { name: h_rt, type: str, scope: scheduler, default: "4:00:00" }
      # scope: scheduler → consumed by the HPC submit_template as
      # {{param.h_rt}}; never injected into {{args}}
defaults:
  gpus_per_task: 1                           # THE way to ask for GPUs (rule 3)
  max_retry: 3
```

**job.yaml** — one SELECTION over the catalog: what varies THIS time.

```yaml
project: resnet50
note: "lr sweep, adam vs sgd"      # human label — labels go here, not in params
sweep:                             # blocks combine via CROSS-PRODUCT — this is
  - method: grid                   #   documented semantics, not a bug
    parameters:                    # grid = cartesian product within the block
      lr: [0.001, 0.01, 0.1]
      optimizer: [adam, sgd]
  - method: list                   # list = zip 1-to-1 (equal lengths required)
    parameters:                    #   → USE THIS for values that belong together
      batch_size: [32, 64]         #     (benchmark+walltime, model+parser)
      num_workers: [4, 8]
# grid(3 × 2) × list(2) = 12 tasks
```

**The mapping test — apply it to every plan before submitting:**
one task = one real unit of work (one training run, one model × config ×
benchmark evaluation). If a value the experiment VARIES shows up as a loop
inside your payload script, the mapping is wrong (tasks become
unretryable/untrackable per value). If you find yourself hand-enumerating
every combination as one flat zip table, the mapping is also wrong —
express structure as grid × zip blocks and let runq expand.

## 5. Known pitfalls

- Every sweep param must be consumed by `command_template` or (via
  `{{param.*}}`) by the HPC `submit_template`; an unconsumed param is a
  submit-time error **by design**. Don't fake consumption: labels belong in
  `note`, scheduler knobs get `scope: scheduler`.
- Never point `command_template` at a script that itself submits jobs or
  loops over swept values, and don't generate wrapper shell scripts —
  placeholders may appear anywhere in the command, including inside quoted
  strings. (A task orchestrating its own INTERNAL processes — e.g. starting
  a local vllm server, waiting, evaluating, tearing down — is fine.)
- Secrets never go in `command_template` or `environment:` — not even the
  placeholder value you found in the legacy script. Put them in
  `working_dir/.env` (add a `.env.example` + `.gitignore` entry) — sourced
  at task start, never stored by runq.
- Task output lands in the task's own log (`runq logs <task_id>
  --no-follow`), not in the scheduler's `-o`/`-e` files.
- Deliver the minimum: yaml + (if needed) one slimmed payload script.
  Do not write progress dashboards (`runq ps` exists), README novels, or
  helper wrappers around runq commands.

## 6. Deep dives — read only when the situation calls for it

| Situation | Read |
|---|---|
| Migrating an existing qsub/sbatch/nohup script into runq | references/migration.md — read it BEFORE proposing any wrapper |
| Installing runq, adding or validating a cluster target | references/setup.md |
| Instrumenting training code: metrics, checkpoints, early-stop, preemption | references/sdk.md |
