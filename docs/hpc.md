# HPC guide — running runq on SLURM / PBS / SGE clusters

On a cluster, runq does **not** replace the scheduler. It compiles your
sweep, writes per-task workspaces, renders one submit command per task
from your template, and delegates scheduling to `sbatch`/`qsub`. No
resident daemon, no root, no admin cooperation — everything runs as your
user, and all scheduler dialect lives in *your* config.

## One-time target setup

```bash
runq target add tsubame --template=tsubame \
    --host=login.t4.gsic.titech.ac.jp --user=alice
runq target check tsubame
runq connect tsubame
```

- `target add` writes a **preset** (slurm | pbs | sge | tsubame | abci)
  into `~/.runq/config.yaml`. Presets are starting points — edit the
  target's `submit_template`, `submit_id_regex`, and `kill_template` to
  match your site (also editable in the dashboard Settings page, with
  placeholder hints and a built-in checker).
- `target check` renders every template with sample values and validates
  placeholders and the id regex. It is free and local — run it after every
  template edit.
- `connect` verifies the host key with you, installs the remote CLI, and
  starts the socket forward. `runq target disconnect` undoes it.

Select the target per command (`-t tsubame`), per session
(`runq target use tsubame`), or persistently (`runq target use -p`).

## Submitting

```bash
runq submit job.yaml --project-file project.yaml -t tsubame --dry
runq submit job.yaml --project-file project.yaml -t tsubame
runq ps                          # target-aware; same commands as local
runq status <job_id>             # refresh + show tasks
runq kill <job_id|task_id>       # cancels via your kill_template
```

## Per-task scheduler knobs (walltime, queue)

Declare them as catalog params with `scope: scheduler`, and reference them
in `submit_template` as `{{param.*}}`:

```yaml
# project.yaml
params:
  - { name: h_rt, type: str, scope: scheduler, default: "4:00:00" }
  - { name: node_kind, type: str, scope: scheduler, default: node_q,
      choices: [node_q, node_h, node_f], strict: true }
```

```yaml
# job.yaml — pair each benchmark with ITS walltime via zip
sweep:
  - method: grid
    parameters: { model: [llama3-8b, qwen2-7b] }
  - method: list
    parameters:
      benchmark: [mmlu, gsm8k, humaneval]
      h_rt: ["4:00:00", "2:00:00", "1:00:00"]
```

`scope: scheduler` params are consumed by the submit command, never by
your training command, and never injected into `{{args}}`. With
`strict: true` a typo'd queue name fails at submit — not after four hours
in queue. One job can carry per-benchmark time limits this way.

`environment:` from project.yaml is injected into every task AND prefixed
onto the submit command, so `$TSUBAME_GROUP`-style references inside
submit_template resolve from project config.

## How runq behaves on the login node

runq is a polite login-node citizen. Task state comes primarily from each
task's `status.json`, written by the generated run.sh at start, at exit,
and — via a signal trap — when the scheduler kills the task (walltime,
`qdel`): those are local file reads, not scheduler queries. The
`status_template` probe only covers tasks that died without last words, is
rate-limited per job (20s floor for the dashboard's automatic polling;
explicit `runq status` always probes), and listing-style templates like
the presets' full `qstat` cost ONE scheduler call per pass regardless of
task count. When nothing asks, nothing polls.

Each task runs **from the project's `working_dir`** (relative paths in
your command just work), with all output redirected to its own log —
`runq logs <task_id>` and the dashboard read it; `-o`/`-e` in your
submit_template only catches scheduler-level noise. Workspaces live under
`.runq/<note>-<job_id>/<task_id>/`.

## Preflight on strict login nodes

Preflight results are three-state (passed / failed / skipped) and label
local checks with their scope — a login node without `nvidia-smi` needs no
special-casing; the GPU check honestly reports "skipped: not applicable
here". For clusters that restrict login-node work, set
`hpc: preflight_local: false` in config rather than `--no-preflight`.

## Debugging a failed submit

1. Read the error — runq errors state the fix.
2. `~/.runq/logs/runq.log` records every submit/kill with the fully
   rendered command and the scheduler's raw output.
3. `runq target check <name>` re-validates templates after any edit.
4. `runq doctor` checks the right paths per mode.
