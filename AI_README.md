# AI_README — runq Setup Guide for AI Agents

> You are helping a user set up runq, a GPU job scheduler for research labs.
> For background, see README.md. For example configs, see examples/.

## Step 0: runq's Boundary (read this FIRST)

runq **is** the loop and **is** the submitter. When migrating an existing
bash pipeline, the most common failure is wrapping it instead of replacing it.

**The mapping rule:** one runq task = one real unit of work (one training
run, one model×benchmark evaluation). The `for` loop / ENV-override layer /
task list at the bottom of the user's script — that's the sweep. The
qsub/sbatch line — that's `hpc: submit_template`. The actual workload
command — that's `command_template`, called **directly**.

**Anti-patterns (all observed in real migrations — do NOT do these):**

1. ❌ `command_template: bash qsub_all.sh ...` where the script itself
   submits jobs → runq submits a job that submits jobs: untrackable,
   unkillable, unretryable. Replace the script's loop with a sweep and call
   the inner workload script directly.
2. ❌ Generating a wrapper `.sh` to adapt arguments → unnecessary.
   `command_template` supports named placeholders anywhere in the command
   (`--task-name {{task_name}}`, even inside quoted strings) plus `{{args}}`
   for the rest. No shim layer. (Distinct from a *slimmed copy* of a real
   payload script, which is encouraged — see Nested scripts below.)
3. ❌ Putting metadata-only fields into the sweep → every param must be
   consumed (by `command_template` OR by `submit_template`); unconsumed
   params are a submit-time error by design. A label belongs in `note`.
   Scheduler knobs (walltime, queue) are params too — declare them with
   `scope: scheduler` in the project catalog and they are consumed by
   `{{param.*}}` in submit_template, never by the command and never via
   `{{args}}`. Do NOT invent fake consumption workarounds.
4. ❌ Hand-rolling per-task scheduler flags (walltime, queue, priority) in
   wrappers → declare them as catalog params with `scope: scheduler`, put
   values in fixed_params or zip columns, and reference them in
   `submit_template` as `{{param.h_rt}}`, `{{param.node_kind}}`.
   Add `strict: true` + `choices` for finite vocabularies (queue names,
   providers) — typos then fail at submit instead of after hours in queue.
5. ❌ Enumerating every concrete task into job.yaml → wrong division of
   labor. **project.yaml is the parameter CATALOG**: `params:` entries
   define `type` / `default` / `choices` / `include` — the Web GUI builds
   its submit form from exactly this. **job.yaml is one SELECTION** over
   that catalog, expressed compactly via `grid` (cross product) and `list`
   (zip). Get the catalog right once; the user then generates job.yaml
   from the GUI (or writes a 10-line one), instead of maintaining a
   35-line task inventory by hand.

**What runq replaces** in a typical lab script: the model loop (→ sweep),
ENV_* override layers (→ sweep), tmux/nohup for local runs (→ daemon mode),
job-name sanitizers (→ project `job_name` template; `{{name}}` in
submit_template is pre-sanitized, never digit-first), qstat watch scripts
(→ `runq hpc status`), pre-download blocks (→ project `setup_command`,
runs once per submit on the login node), `.env` sourcing (→ automatic:
`working_dir/.env` is sourced at task start; explicit env always wins;
runq never stores its values).

**Editing configs:** `project.yaml` is the source of truth — hand-edit it
directly; runq picks the change up on the next selection/submit (the DB is
a self-healing cache). No re-registration command needed after edits.

**What stays with the user:** the workload script itself, the scheduler
dialect (templates in `~/.runq/config.yaml`), and the results/aggregation
pipeline. runq parses no scheduler output beyond `submit_id_regex` and the
optional `status_parser`.

**Nested scripts** (a submitter script that qsubs a payload script): judge
each layer of the payload by one question — *is it serving the scheduler,
or serving the experiment?*

- Serving the scheduler → runq replaces it: stdout/stderr tee/redirect
  hacks, per-job log naming, custom job IDs, tmux/local-run branches,
  manual `CUDA_VISIBLE_DEVICES`, required-arg checks that duplicate
  preflight. Delete these layers from the runq path.
- Serving the experiment → keep it, unchanged: module loads, starting and
  stopping a local inference server *inside* the task, the evaluation
  itself, per-task result aggregation. A task orchestrating its own
  internal processes is fine — the anti-pattern is a task *submitting jobs*.

When the slimming is substantial, **do not edit the original script in
place** — other people and the legacy path still use it. Copy it to a
slimmed runq-only variant (e.g. `scripts/runq/<name>.sh`): runq absorbs so
much of the logic that the copy stays short, readable, and fast to debug.

**Migration recipe** for a typical `for MODEL in ...; do qsub ...` script:
read the script bottom-up — the task list / loop variables become sweep
parameters (zip columns for paired values like task+walltime), the qsub
flags become `submit_template` (per-task flags via `{{param.*}}`), the env
exports become project `environment` (secrets → `.env`), any pre-download
block becomes `setup_command`, and the innermost command line becomes
`command_template`. Everything else in the script is scaffolding that
runq replaces.

## Step 1: Get runq

You will typically receive only the repo URL. runq has three installable
pieces — install what the user actually needs, not everything:

| Piece | When needed | How |
|---|---|---|
| **CLI core** (always) | every setup | `go install <repo>/cmd/runq@latest` — no clone needed. No Go? Download a release binary, or clone + `go build -o runq ./cmd/runq` |
| **Dashboard** (optional) | user wants the web GUI (submit form, monitoring, config editing) | a `runq-dashboard-*` binary from GitHub Releases; or build from source: clone → `cd web/dashboard && yarn install && yarn build` → `go build -tags dashboard -o runq ./cmd/runq` |
| **Python SDK** (optional) | user's training script wants metrics reporting / safe checkpointing / early-stop | clone → `cd sdk/python && pip install -e .` |

Decision guide: start with CLI core only — it covers submit/status/kill on
both daemon and HPC. Add the dashboard if the user mentions a GUI or
monitoring. Add the SDK only when instrumenting training code; plain
scripts run fine without it.

Put the binary on PATH (outside any synced/cloud folder — macOS kills
ad-hoc-signed binaries that sync tools touch). Verify with `runq --help`.

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

1. `runq hpc init --scheduler <slurm|pbs|sge|tsubame|abci>`
   (also sets `mode: hpc` when mode was unset)
2. Edit templates: `runq hpc config edit`, then **always**
   `runq hpc config check` — renders every template with sample values,
   validates placeholders and the submit_id_regex capture group, zero cost.
3. Write `project.yaml` and `job.yaml` (same format as daemon).
   Per-task scheduler knobs (h_rt, queue) = catalog params declared
   `scope: scheduler` (+ `strict: true` with `choices` when the vocabulary
   is finite), referenced via `{{param.*}}` in submit_template.
4. `runq hpc submit job.yaml --project-file project.yaml --dry-run`
   → shows preflight (three-state), the rendered submit command and run.sh,
   writes nothing. Confirm with the user, then drop `--dry-run`.

Useful: `note: "{{model}}-{{version}}"` auto-numbers re-runs (-v2, -v3);
`runq hpc status <id> --json` includes a `capabilities` block for scripting.

## Where to Find Answers

Do **not** read runq's source code — everything you need is in the
user-facing surfaces below, and source-diving wastes time.

| When you need to... | Use |
|---|---|
| Understand project.yaml schema | `examples/project.yaml` + README "Configuration" |
| Understand job.yaml / sweep syntax | `examples/job.yaml`, `examples/job_simple.yaml` |
| Understand HPC config fields | comments in the generated `~/.runq/config.yaml`, then `runq hpc config check` |
| See all CLI commands and flags | `runq --help`, `runq hpc --help`, `runq <cmd> --help` |
| Validate anything | `runq hpc config check`, `runq hpc submit --dry-run`, `runq doctor` |
| Inspect machine-readable state | `runq hpc status <id> --json`, `runq hpc ls --json` |

## Python SDK (`sdk/python/runq/`)

runq includes a Python SDK that users `import runq` inside their training scripts. It handles parameter injection, metrics logging, checkpoint safety, preemption, and early stopping.

### Architecture

- **Tri-mode**: daemon (Unix socket to resident daemon), no_daemon (file-only, for HPC), manual (no runq infrastructure).
- **Context** (`_context.py`): `runq.context()` initializes from env vars (`RUNQ_TASK_ID`, `RUNQ_SOCKET`, etc.) or params passed directly. Returns a `Context` dataclass.
- **ParamDict** (`_context.py`): dict subclass with fuzzy-match suggestions on KeyError (difflib).
- **Seed** (`_context.py`): `runq.seed` — deterministic per-task seed via SHA-256 of task_id, mod 2^32.

### Key modules

| File | Purpose |
|---|---|
| `_config.py` | `@runq.dataclass` — typed param class with auto-merge from sweep params |
| `_range.py` | `runq.range()` + shared iterator core (`_check_break`, `_init_iterator`) |
| `_loop.py` | `runq.loop()` for arbitrary iterables + `@epoch` + `log_group()` |
| `_safe_save.py` | Atomic checkpoint writes + ENOSPC freeze flow + decorator form |
| `_manifest.py` | Checkpoint manifest for `keep_last_n` / `keep_best` cleanup |
| `_report.py` | `runq.report()` — early-stop evaluation with pluggable policies |
| `_policies.py` | Built-in policies: `patience`, `threshold`, `convergence` |
| `_events.py` | `log_metric()` + jsonl event appender |
| `_transport.py` | httpx Unix socket client for daemon communication |
| `_sync.py` | `sync_now()` — push metrics to daemon on demand |

### SDK installation

```bash
cd sdk/python && pip install -e .
```

### Key files for SDK work

Same rule as above: don't read SDK source. Use:

| When you need to... | Use |
|---|---|
| Understand the public API | `sdk/python/examples/` + README "Python SDK" section |
| See usage patterns | `sdk/python/examples/` |

## Pitfalls

- Daemon mode needs `nvidia-smi` on PATH and the daemon running.
- HPC `runq hpc init` writes a **template** — user must customize it, then
  validate with `runq hpc config check`.
- On HPC, login node may differ from compute node — preflight results are
  three-state (passed/failed/skipped) and label local checks with their
  scope; set `hpc: preflight_local: false` for strict login nodes instead
  of `--no-preflight`.
- Secrets: never put tokens in `command_template`/`environment` — put them
  in `working_dir/.env` (sourced at task start, never stored by runq).
- `working_dir` in project.yaml must exist at submit time.
- SDK `runq.safe_save()` uses `_resolve_mountpoint` on the resolved absolute path — relative paths are resolved via `ctx.checkpoint_dir` first.
- SDK manifest cleanup is scoped to files the SDK created — user-placed files are never touched.
