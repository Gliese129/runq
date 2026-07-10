# SKILLS.md — runq

> You are helping a user set up **runq**, a GPU job scheduler for research labs.
> This file is a self-contained skill: read it, then help the user.
> For product overview see `README.md`. For design rationale see `docs/design_philosophy.md`.

---

## 1. runq's Boundary — read this first

runq **is** the loop and **is** the submitter.
The most common migration failure is wrapping an existing pipeline instead of
replacing it.

**The mapping rule:** one runq task = one real unit of work (one training run,
one model x benchmark evaluation).

- The user's `for` loop / ENV overrides / task list at the bottom of the script
  — that's the **sweep** (job.yaml).
- The `qsub` / `sbatch` line — that's the target's **`submit_template`**.
- The actual workload command — that's **`command_template`**, called directly.

**What runq replaces** in a typical lab script:

| Legacy pattern | runq equivalent |
|---|---|
| Model loop / ENV overrides | sweep (grid / list in job.yaml) |
| tmux / nohup for local runs | `runq daemon start --detach` |
| Job-name sanitisers | `job_name` template in project.yaml (`{{name}}` is pre-sanitised) |
| qstat watch scripts | `runq status` / dashboard |
| Pre-download blocks | `setup_command` in project.yaml (runs once per submit) |
| `.env` sourcing | Automatic: `working_dir/.env` sourced at task start |

**What stays with the user:** the workload script itself, the scheduler dialect
(templates in `~/.runq/config.yaml`), and the results/aggregation pipeline.

---

## 2. Anti-Patterns

All of these have been observed in real migrations. Do NOT do them.

### 2.1 Submitting a script that itself submits jobs

`command_template: bash qsub_all.sh ...` where the script internally submits
jobs — runq submits a job that submits jobs: untrackable, unkillable,
unretryable. Replace the script's loop with a sweep and call the inner workload
directly.

### 2.2 Generating wrapper shell scripts

`command_template` supports named placeholders anywhere in the command
(`--task-name {{task_name}}`, even inside quoted strings) plus `{{args}}` for
the rest. No shim layer is needed. (A *slimmed copy* of a real payload script
is fine — see Nested Scripts below.)

### 2.3 Putting metadata-only fields into the sweep

Every sweep param must be consumed by `command_template` OR by
`submit_template`; unconsumed params are a submit-time error by design. Labels
belong in `note`. Scheduler knobs (walltime, queue) are params declared with
`scope: scheduler` and consumed via `{{param.*}}` in submit_template — never
by the command, never via `{{args}}`. Do NOT invent fake consumption workarounds.

### 2.4 Hand-rolling per-task scheduler flags in wrappers

Declare them as catalog params with `scope: scheduler`, put values in
`fixed_params` or zip columns, and reference them in `submit_template` as
`{{param.h_rt}}`, `{{param.node_kind}}`. Add `strict: true` + `choices` for
finite vocabularies (queue names, providers) — typos then fail at submit
instead of after hours in queue.

### 2.5 Enumerating every concrete task into job.yaml

Wrong division of labour. **project.yaml is the parameter CATALOG**: `params:`
entries define `type` / `default` / `choices` / `include` — the Web GUI builds
its submit form from exactly this. **job.yaml is one SELECTION** over that
catalog, expressed compactly via `grid` (cross product) and `list` (zip). Get
the catalog right once; the user then generates job.yaml from the GUI or writes
a short one, instead of maintaining a task inventory by hand.

---

## 3. Nested Scripts

When a submitter script calls a payload script, judge each layer by one
question: **is it serving the scheduler, or serving the experiment?**

**Serving the scheduler → runq replaces it:**
stdout/stderr tee/redirect hacks, per-job log naming, custom job IDs,
tmux/local-run branches, manual `CUDA_VISIBLE_DEVICES`, required-arg checks
that duplicate preflight. Delete these layers from the runq path.

**Serving the experiment → keep it unchanged:**
module loads, starting a local inference server *inside* the task, the
evaluation itself, per-task result aggregation. A task orchestrating its own
internal processes is fine — the anti-pattern is a task *submitting jobs*.

When the slimming is substantial, **copy** the script to a runq-only variant
(e.g. `scripts/runq/<name>.sh`) rather than editing the original — other people
and the legacy path still use it.

---

## 4. Migration Recipe

Read the user's existing script **bottom-up**:

1. **Task list / loop variables** → sweep parameters.
   Paired values (task + walltime) become zip columns, not separate grids.
2. **qsub / sbatch flags** → `submit_template`.
   Per-task flags via `{{param.*}}` (declare `scope: scheduler`).
3. **ENV exports** → `environment:` in project.yaml.
   Secrets → `.env` in `working_dir` (never stored by runq).
4. **Pre-download blocks** → `setup_command` (runs once on the login node).
5. **Innermost command** → `command_template`.
6. **Everything else** is scaffolding that runq replaces. Delete it from the
   runq path.

---

## 5. Setup Guide

### Step 1 — Install

runq has three installable pieces. Install only what the user needs:

| Piece | When | How |
|---|---|---|
| CLI core | Always | `go install .../cmd/runq@latest`, or download a release binary |
| Dashboard | User wants web GUI | `runq-dashboard-*` from Releases; or build from source (see README) |
| Python SDK | Training script needs metrics / checkpoints / early-stop | `pip install runq` or `cd sdk/python && pip install -e .` |

Start with CLI core only. Add the dashboard if the user mentions a GUI.
Add the SDK only when instrumenting training code.

### Step 2 — Align with the User

Before touching any config, ask:

1. **Environment** — local GPU machine, or HPC cluster (Slurm / PBS / SGE)?
2. **Training command** — what does their invocation look like?
3. **Sweep parameters** — which hyperparameters, what values, grid or list?
4. **GPU count** — how many GPUs per task?
5. **Storage** (HPC only) — default `.runq/` or a custom path?
6. **Cluster details** (HPC only) — which scheduler, any special submit flags?

### Step 3 — Configure & Submit

**Daemon mode (local):**

```
runq doctor                    # verify GPU environment
runq daemon start --detach
# write project.yaml + job.yaml (see examples/)
runq project add .
runq submit --dry-run .        # confirm with user
runq submit .
```

**HPC cluster (remote target):**

```
runq target add <name> --template=<slurm|pbs|sge|tsubame|abci> \
     --host=<login-node> --user=<user>
runq target check <name>       # renders templates, validates placeholders
runq connect <name>            # verify SSH, install remote CLI, start forward
# write project.yaml + job.yaml
runq submit job.yaml --project-file project.yaml --target <name> --dry-run
runq submit job.yaml --project-file project.yaml --target <name>
```

After `runq target add`, always `runq target check` — it renders every
template with sample values, validates placeholders and `submit_id_regex`,
zero cost.

---

## 6. Config Editing

`project.yaml` is the source of truth — hand-edit it directly; runq picks the
change up on the next selection / submit (the DB is a self-healing cache). No
re-registration command needed.

---

## 7. Python SDK — Essentials

The SDK (`import runq`) integrates with training scripts for parameter
injection, metrics logging, checkpoint safety, preemption, and early stopping.

**Tri-mode:** daemon (Unix socket), no_daemon (file-only for HPC),
manual (no runq infrastructure).

Key API surface:

- `runq.context()` — initialise from env vars or params
- `@runq.dataclass` — typed param class with auto-merge from sweep params
- `runq.range()` / `runq.loop()` — iterators with preemption + early-stop
- `runq.safe_save()` — atomic checkpoint writes + ENOSPC handling
- `runq.report()` — early-stop evaluation with pluggable policies
- `runq.seed` — deterministic per-task seed (SHA-256 of task_id)

For architecture details and module map, see `docs/sdk_reference.md`.

---

## 8. Where to Find Answers

Do **not** read runq's source code — everything you need is in user-facing
surfaces:

| When you need to... | Use |
|---|---|
| Understand project.yaml schema | `examples/project.yaml` + README "Configuration" |
| Understand job.yaml / sweep syntax | `examples/job.yaml`, `examples/job_simple.yaml` |
| Understand target config fields | Comments in `~/.runq/config.yaml`, then `runq target check` |
| See all CLI commands and flags | `runq --help`, `runq <cmd> --help` |
| Validate anything | `runq target check`, `runq submit --dry-run`, `runq doctor` |
| Debug a failed submit | `~/.runq/logs/runq.log` |
| Inspect machine-readable state | `runq status <id> --json`, `runq ps --json` |
| SDK usage patterns | `sdk/python/examples/` + README "Python SDK" |
| Design philosophy | `docs/design_philosophy.md` |

---

## 9. Pitfalls

- Daemon mode needs `nvidia-smi` on PATH and the daemon running.
- `runq target add` fills a **preset** — user must customise it, then validate
  with `runq target check <name>`.
- On HPC, the login node may differ from compute nodes — preflight results are
  three-state (passed / failed / skipped) and label local checks with their
  scope. Set `hpc: preflight_local: false` for strict login nodes instead of
  `--no-preflight`.
- Task output goes to the task's own log (`runq logs <task_id>`), NOT to the
  scheduler's `-o` / `-e` files.
- Job/task IDs are typed (`jb...` / `tk...`); never parse workspace paths,
  read them from `--json` output.
- Secrets: never put tokens in `command_template` / `environment` — put them
  in `working_dir/.env`.
- `working_dir` in project.yaml must exist at submit time.
- SDK `runq.safe_save()` resolves relative paths via `ctx.checkpoint_dir`.
- SDK manifest cleanup is scoped to files the SDK created — user-placed files
  are never touched.
