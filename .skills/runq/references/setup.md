# Installing runq and configuring environments

## Install — four pieces, install only what's needed

| Piece | When | How |
|---|---|---|
| CLI/client core | Always | `curl -fsSL https://raw.githubusercontent.com/Gliese129/runq/main/install.sh \| sh` |
| Dashboard | User wants a web GUI | answer yes in the installer, or set `RUNQ_WITH_UI=1` |
| Linux executor (`runqd`) | Linux local GPU machine | installed automatically next to `runq`; not installed on macOS |
| Python SDK | Training script needs metrics / checkpoints / early-stop | `pip install runq-sdk` (import name: `runq`) |

The installer supports Linux/macOS and amd64/arm64, verifies checksums, and
starts the client daemon in the background. For non-interactive use set
`RUNQ_WITH_UI=0` or `1`; use `RUNQ_START_DAEMON=0` to skip startup.
Linux local execution needs `nvidia-smi` on PATH. macOS/HPC client mode
needs only the relevant SSH or cluster CLI. Verify any setup with
`runq doctor`.

## Align with the user before touching config

1. Environment — local GPU machine, or HPC cluster (Slurm / PBS / SGE)?
2. Training command — what does their invocation look like?
3. Sweep parameters — which hyperparameters, what values, grid or list?
4. GPU count per task?
5. (HPC) storage: default `.runq/` or a custom `data_path`?
6. (HPC) which scheduler, any special submit flags?

## Local GPU machine (daemon mode)

```
runq doctor                    # verify GPU environment
runq daemon start -d           # installer already starts it
# write project.yaml + job.yaml (or runq init <script.py>)
runq project add .
runq submit . --dry            # confirm with the user
runq submit .
```

## HPC cluster (remote target)

```
runq target add <name> --template=<slurm|pbs|sge|tsubame|abci> \
     --host=<login-node> --user=<user>
runq target check <name>       # renders templates, validates placeholders — free
runq connect <name>            # verify SSH + host key, install remote CLI, start forward
# write project.yaml + job.yaml
runq submit job.yaml --project-file project.yaml -t <name> --dry
runq submit job.yaml --project-file project.yaml -t <name>
```

`runq target add` fills a **preset** — the user must customise
`submit_template`, `submit_id_regex`, and `kill_template` in
`~/.runq/config.yaml` to match their cluster, then validate with
`runq target check <name>` (renders every template with sample values,
zero cost). Always check after any template edit.

Per-task scheduler knobs (walltime, queue) live in the sweep as params with
`scope: scheduler` and are referenced from `submit_template` as
`{{param.h_rt}}`, `{{param.node_kind}}`.

`environment:` from project.yaml is injected into every task AND prefixed
onto the HPC submit command, so `$MY_GROUP`-style references in
submit_template resolve from project config.

## Environment pitfalls

- On HPC the login node differs from compute nodes — preflight results are
  three-state (passed / failed / skipped) and label local checks with their
  scope. For strict login nodes set `hpc: preflight_local: false` rather
  than `--no-preflight`.
- Task output goes to the task's own log (`runq logs <task_id> --no-follow`), NOT to the
  scheduler's `-o` / `-e` files (those only catch scheduler-level noise).
- Python envs (uv / venv / conda) are auto-detected during `runq init` and
  activated per task; override in project.yaml if detection picks wrong.
- File locations: global config `~/.runq/config.yaml`; operation log
  `~/.runq/logs/runq.log`; per-task workspaces under
  `<working_dir>/.runq/<note>-<job_id>/<task_id>/`.
