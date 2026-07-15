# Migrating an existing pipeline into runq

Read this before touching any legacy submit script. The most common migration
failure is wrapping the existing pipeline instead of replacing it.

## The boundary rule

runq **is** the loop and **is** the submitter.
**One runq task = one real unit of work** (one training run, one
model × benchmark evaluation).

- The user's `for` loop / ENV overrides / task list → the **sweep** (job.yaml)
- The `qsub` / `sbatch` line → the target's **`submit_template`**
- The actual workload command → **`command_template`**, called directly

What runq replaces in a typical lab script:

| Legacy pattern | runq equivalent |
|---|---|
| Model loop / ENV overrides | sweep (grid / list in job.yaml) |
| tmux / nohup for local runs | `runq daemon start --detach` |
| Job-name sanitisers | `job_name` template in project.yaml (`{{name}}` is pre-sanitised) |
| qstat watch scripts | `runq status` / dashboard |
| Pre-download blocks | `setup_command` in project.yaml (runs once per submit; may reference **fixed** params only) |
| `.env` sourcing | Automatic: `working_dir/.env` sourced at task start |

What stays with the user: the workload script itself, the scheduler dialect
(templates in `~/.runq/config.yaml`), and the results/aggregation pipeline.

## Anti-patterns (all observed in real migrations — do NOT do these)

1. **Submitting a script that itself submits jobs.**
   `command_template: bash qsub_all.sh` makes runq submit a job that submits
   jobs: untrackable, unkillable, unretryable. Replace the script's loop with
   a sweep and call the inner workload directly.
2. **Generating wrapper shell scripts.**
   `command_template` supports named placeholders anywhere in the command
   (`--task-name {{task_name}}`, even inside quoted strings) plus `{{args}}`
   for the rest. No shim layer is needed. (A *slimmed copy* of a real payload
   script is fine — see Nested scripts below.)
3. **Putting metadata-only fields into the sweep.**
   Every sweep param must be consumed by `command_template` OR by
   `submit_template`; unconsumed params are a submit-time error by design.
   Labels belong in `note`. Scheduler knobs (walltime, queue) are params
   declared with `scope: scheduler` and consumed via `{{param.*}}` in
   submit_template — never by the command, never via `{{args}}`. Do NOT
   invent fake consumption workarounds.
4. **Hand-rolling per-task scheduler flags in wrappers.**
   Declare them as catalog params with `scope: scheduler`, put values in
   fixed params or zip columns, and reference them in `submit_template` as
   `{{param.h_rt}}`, `{{param.node_kind}}`. Add `strict: true` + `choices`
   for finite vocabularies (queue names, providers) — typos then fail at
   submit instead of after hours in queue.
5. **Enumerating every concrete task into job.yaml.**
   Wrong division of labour. project.yaml is the parameter CATALOG (`params:`
   with `type` / `default` / `choices` / `include`); job.yaml is one compact
   SELECTION over it via `grid` (cross product) and `list` (zip). A single
   hand-flattened 10-row zip table where grid(2) × zip(5) would do is this
   anti-pattern in disguise. Get the catalog right once instead of
   maintaining a task inventory by hand.
6. **Doing the scheduler's job in YAML.**
   GPU indices as a sweep param, tasks pre-assigned to "slots", predicting
   which task runs when — all wrong, and they actively conflict with runq's
   own GPU assignment at runtime. `gpus_per_task` expresses the need; runq
   decides placement and order.

## Nested scripts

When a submitter script calls a payload script, judge each layer by one
question: **is it serving the scheduler, or serving the experiment?**

- Serving the scheduler → runq replaces it: stdout/stderr tee/redirect hacks,
  per-job log naming, custom job IDs, tmux/local-run branches, manual
  `CUDA_VISIBLE_DEVICES`, required-arg checks duplicating preflight.
  Delete these layers from the runq path.
- Network ports deserve special care: a legacy script that runs one server
  at a time can hardcode a port; under runq, several tasks run
  CONCURRENTLY on one machine. Derive a free port inside the payload at
  runtime (e.g. bind port 0 and read it back) — never keep the hardcoded
  port and never assign ports per-task in the sweep.
- Serving the experiment → keep it unchanged: module loads, starting a local
  inference server *inside* the task, the evaluation itself, per-task result
  aggregation. A task orchestrating its own internal processes is fine — the
  anti-pattern is a task *submitting jobs*.

When the slimming is substantial, **copy** the script to a runq-only variant
(e.g. `scripts/runq/<name>.sh`) rather than editing the original — other
people and the legacy path still use it.

## Migration recipe — read the legacy script bottom-up

1. Task list / loop variables → sweep parameters.
   **Paired values (e.g. benchmark + its walltime) become zip columns
   (`method: list`), not separate grids.**
2. qsub / sbatch flags → `submit_template`. Per-task flags via `{{param.*}}`
   (declare `scope: scheduler`).
3. ENV exports → `environment:` in project.yaml.
   Secrets → `.env` in `working_dir` (never stored by runq).
4. Pre-download blocks → `setup_command` (runs once on the login node,
   fixed params only — if it loops over swept values, either download the
   full set unconditionally or leave it as a documented manual step).
5. Innermost command → `command_template`.
6. Everything else is scaffolding that runq replaces. Delete it from the
   runq path.

Finish with `runq submit <file> --dry` and show the user the expansion before
any real submit.
