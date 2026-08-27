"""SDK Context object + initialization.

Three modes (see design doc §"Mode matrix"):

* daemon     — RUNQ_TASK_ID set + RUNQ_SOCKET_PATH set + RUNQ_NO_DAEMON unset
* no_daemon  — RUNQ_TASK_ID set + (RUNQ_SOCKET_PATH unset OR RUNQ_NO_DAEMON=1)
* manual     — RUNQ_TASK_ID unset (running outside runq, local debug)

The module-level singleton `_ctx` is set by `context()` and consumed by
other SDK modules. Users get the Context back from `context()` and can
also call `get_ctx()` (mainly for internal use).
"""
from __future__ import annotations

import json
import os
import uuid
import difflib
import hashlib
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

# Module-level singleton. None until context() is called.
_ctx: Context | None = None


class ParamDict(dict):
    """Dict subclass with helpful KeyError messages for parameter access.

    When a key is missing, shows all available keys and suggests close
    matches via fuzzy matching — catches the common "sweep YAML says
    ``learning_rate``, code says ``lr``" mismatch immediately.
    """

    def __getitem__(self, key):
        try:
            return super().__getitem__(key)
        except KeyError:
            available = sorted(self.keys())
            close = difflib.get_close_matches(str(key), available, n=3, cutoff=0.5)
            msg = f"'{key}' not found in runq.params.\nAvailable: {available}"
            if close:
                msg += f"\nDid you mean: {close}?"
            raise KeyError(msg) from None


@dataclass
class Context:
    """Per-task runtime context.

    Populated from `RUNQ_*` env vars (daemon-injected) plus a local
    `params.json`. Lazy users mostly interact via module-level helpers
    (`runq.log_metric`, `runq.report`, ...) which pull state from the
    singleton; this struct is the canonical home of all SDK state.

    Fields are kept descriptive on purpose — debugging from `print(ctx)`
    should be enough to see where the SDK thinks it lives.
    """

    mode: str                                 # "daemon" / "no_daemon" / "manual"
    task_id: str
    job_id: str = ""
    project: str = ""

    # Filesystem paths (None when the relevant env var wasn't set).
    task_dir: Path | None = None
    checkpoint_dir: Path | None = None
    metrics_file: Path | None = None
    # Three-file contract (RQ2-1): results.jsonl (runq.record data plane)
    # and events.jsonl (lifecycle plane) live NEXT TO metrics.jsonl. Both
    # are derived from metrics_file rather than env-injected, so an older
    # daemon that only sets RUNQ_METRICS_FILE still yields correct paths.
    results_file: Path | None = None
    events_file: Path | None = None

    # daemon-only fields
    socket_path: str | None = None

    # SDK contract: daemon always injects these, so we read them with a
    # default for the manual mode (where there's no daemon).
    safety_factor_percent: int = 110
    safety_extra_gb: int = 0

    # Loaded from RUNQ_PARAMS_FILE (or local params.json in manual mode).
    params: dict = field(default_factory=dict)

    # ---- step bookkeeping (step 6) ----
    #
    # Single source of truth for "what step is this?". Maintained by:
    # - ``report(metrics, step=N)`` — explicit step writes back here.
    # - ``log_metric(..., step=N)`` — explicit step writes back here too,
    #   so the two helpers stay in sync.
    # - ``loop()`` (step 8) — yields & sets per iteration.
    #
    # When None, log_metric / report write step=None to jsonl. There is
    # NO auto-increment counter (unlike the early step 1 implementation);
    # if you want incrementing steps without loop(), pass step explicitly.
    current_step: int | None = None

    # Deterministic seed derived from task_id. Different per task in the
    # same sweep, but reproducible across reruns of the same task.
    seed: int = 0

    # ---- step-8 loop coordination ----
    #
    # Most-recent Decision returned by ``runq.report``. Read by
    # ``runq.loop()`` after each iteration to decide whether to break.
    # ``Any`` to dodge a forward ref to ``_report.Decision`` (would
    # require a TYPE_CHECKING-only import dance).
    _last_decision: Any | None = None

    # ---- convenience accessors ----

    def get(self, name: str, default: Any = None) -> Any:
        """Look up a param with a fallback default.

        Equivalent to ``ctx.params.get(name, default)`` — common enough to
        deserve a shorthand.
        """
        return self.params.get(name, default)

    # ---- wandb-like shape helpers (F9) ----
    #
    # SDK never imports wandb. These properties produce dicts shaped to
    # be splatted into ``wandb.init(**...)`` / merged into
    # ``wandb.log(...)``. User owns the wandb calls themselves.
    #
    # Kept inline (rather than delegated to a sibling module) so the
    # only lazy import is the one ``wandb_like_metric`` needs for
    # ``_report.latest_report_metrics``. No ``wandb`` import anywhere.

    @property
    def wandb_like_cfg(self) -> dict:
        """Kwargs dict for ``wandb.init(**ctx.wandb_like_cfg)``. See F9/P6.

        When ``RUNQ_SWEEP_KEYS`` is set (P6 W&B integration), the run
        name is built from sweep parameter key=value pairs so each task
        in a sweep is immediately distinguishable in the W&B UI.

        When ``RUNQ_JOB_NOTE`` is set, it becomes the W&B group — giving
        the user's note semantics as the grouping label instead of a raw
        job id.

        Shape::

            {
                "project": self.project or "runq",
                "name":    "lr=0.01_bs=32" or f"{job_id}/{task_id}",
                "group":   job_note or job_id or None,
                "config":  dict(self.params),
            }

        ``entity`` / ``mode`` are intentionally NOT included — those
        come from the user's ``WANDB_*`` env / ``~/.netrc`` and runq
        won't second-guess.
        """
        sweep_keys_raw = os.environ.get("RUNQ_SWEEP_KEYS", "")
        sweep_keys = [k.strip() for k in sweep_keys_raw.split(",") if k.strip()] if sweep_keys_raw else []
        job_note = os.environ.get("RUNQ_JOB_NOTE", "")

        if sweep_keys and self.params:
            parts = [f"{k}={self.params[k]}" for k in sweep_keys if k in self.params]
            name = "_".join(parts) if parts else (f"{self.job_id}/{self.task_id}" if self.job_id else self.task_id)
        else:
            name = f"{self.job_id}/{self.task_id}" if self.job_id else self.task_id

        group = job_note if job_note else (self.job_id or None)

        return {
            "project": self.project or "runq",
            "name": name,
            "group": group,
            "config": dict(self.params or {}),
        }

    @property
    def wandb_like_metric(self) -> dict:
        """Latest reported metrics for ``wandb.log({**ctx.wandb_like_metric, ...})``.

        Empty dict if no report has happened yet. Caller merges their
        own extras at the call site::

            wandb.log({**ctx.wandb_like_metric, "lr": lr}, step=epoch)

        The returned dict is a copy — runq's internal history is not
        exposed to mutation.
        """
        # Lazy import to avoid context ↔ report cycle (_report imports
        # from _context at module load).
        from . import _report
        return _report.latest_report_metrics()


# ---- mode detection ----

def _truthy(s: str | None) -> bool:
    """Lenient truthy parse for env vars. Empty / "0" / "false" → False."""
    if not s:
        return False
    return s.strip().lower() not in ("0", "false", "no", "off")


def _detect_mode() -> str:
    """Resolve which mode the SDK is operating in.

    Order matters:
    1. No RUNQ_TASK_ID → user is running outside runq → manual.
    2. RUNQ_NO_DAEMON=1 (explicit HPC opt-out) → no_daemon.
    3. No socket path → daemon isn't reachable → no_daemon.
    4. Otherwise daemon.
    """
    if not os.environ.get("RUNQ_TASK_ID"):
        return "manual"
    if _truthy(os.environ.get("RUNQ_NO_DAEMON")):
        return "no_daemon"
    if not os.environ.get("RUNQ_SOCKET_PATH"):
        return "no_daemon"
    return "daemon"


def _load_params(path: Path | None) -> dict:
    """Strict params.json loader. Missing file → empty dict; broken file
    → RuntimeError. We surface load failures because silent {} would let
    the user run with the wrong hyperparameters and only notice hours later.
    """
    if path is None or not path.exists():
        return {}
    try:
        with open(path, encoding="utf-8") as f:
            data = json.load(f)
    except (OSError, json.JSONDecodeError) as e:
        raise RuntimeError(f"runq: failed to load params from {path}: {e}") from e
    if not isinstance(data, dict):
        # Out of the try block on purpose — this is a structural error,
        # not a parse failure, and we want the message to be specific.
        raise RuntimeError(
            f"runq: params file {path} must contain a JSON object, "
            f"got {type(data).__name__}"
        )
    return data


def _int_env(name: str, default: int) -> int:
    """Parse an int env var with a default. Returns default on parse error."""
    raw = os.environ.get(name)
    if raw is None or raw == "":
        return default
    try:
        return int(raw)
    except ValueError:
        return default


def context() -> Context:
    """Initialize the runq SDK context.

    Idempotent: calling twice returns the same singleton. Reads
    ``RUNQ_*`` env vars, loads ``params.json``, sets up the
    ``metrics.jsonl`` sink, and stores the singleton for other SDK
    modules to consume.

    Returns the :class:`Context`. Lazy users usually do not need to keep
    the return value because module-level helpers (``runq.log_metric``,
    ``runq.report``) read the singleton themselves.

    Modes:

    * ``daemon`` — talks to a runq daemon over a unix socket.
    * ``no_daemon`` — HPC mode; daemon not reachable. Writes jsonl
      locally, ``safe_save`` will raise ``RunqDiskFullError`` on disk
      pressure instead of freezing.
    * ``manual`` — user is running their script directly without runq.
      jsonl goes to ``./runq_metrics.jsonl``, task IDs are local UUIDs,
      no daemon comm. Useful for IDE debugging.
    """
    global _ctx
    if _ctx is not None:
        return _ctx

    mode = _detect_mode()
    env = os.environ

    if mode == "manual":
        # Local mode: generate a short id, write jsonl into cwd, look for
        # a sibling params.json (optional — most local debug runs have
        # nothing).
        task_id = "manual-" + uuid.uuid4().hex[:8]
        task_dir: Path | None = None
        checkpoint_dir: Path | None = None
        metrics_file: Path | None = Path.cwd() / "runq_metrics.jsonl"
        results_file: Path | None = Path.cwd() / "runq_results.jsonl"
        events_file: Path | None = Path.cwd() / "runq_events.jsonl"
        local_params = Path.cwd() / "params.json"
        params = _load_params(local_params) if local_params.exists() else {}
    else:
        # daemon / no_daemon: daemon injects all the RUNQ_* vars.
        task_id = env["RUNQ_TASK_ID"]
        task_dir = Path(env["RUNQ_TASK_DIR"]) if env.get("RUNQ_TASK_DIR") else None
        checkpoint_dir = (
            Path(env["RUNQ_CHECKPOINT_DIR"]) if env.get("RUNQ_CHECKPOINT_DIR") else None
        )
        metrics_file = (
            Path(env["RUNQ_METRICS_FILE"]) if env.get("RUNQ_METRICS_FILE") else None
        )
        # Siblings of metrics.jsonl (see Context field comment). The daemon's
        # reap reads the same fixed names via workspace.ResultsPath/EventsPath.
        results_file = metrics_file.with_name("results.jsonl") if metrics_file else None
        events_file = metrics_file.with_name("events.jsonl") if metrics_file else None
        params_file = env.get("RUNQ_PARAMS_FILE")
        params = _load_params(Path(params_file)) if params_file else {}

    _ctx = Context(
        mode=mode,
        task_id=task_id,
        job_id=env.get("RUNQ_JOB_ID", ""),
        project=env.get("RUNQ_PROJECT_NAME", ""),
        task_dir=task_dir,
        checkpoint_dir=checkpoint_dir,
        metrics_file=metrics_file,
        results_file=results_file,
        events_file=events_file,
        socket_path=env.get("RUNQ_SOCKET_PATH"),
        safety_factor_percent=_int_env("RUNQ_SAFETY_FACTOR_PERCENT", 110),
        safety_extra_gb=_int_env("RUNQ_SAFETY_EXTRA_GB", 0),
        params=ParamDict(params),
        seed=int(hashlib.sha256(task_id.encode()).hexdigest(), 16) % (2**32),
    )

    # Make sure dirs we will write to actually exist. Cheap, idempotent,
    # and prevents a confusing first-write failure mid-training.
    if _ctx.metrics_file is not None:
        _ctx.metrics_file.parent.mkdir(parents=True, exist_ok=True)
    if _ctx.checkpoint_dir is not None:
        _ctx.checkpoint_dir.mkdir(parents=True, exist_ok=True)

    # Step 7: if the user's job config carried an ``early_stopping:``
    # block, build the matching built-in policy and register it.
    # Done LAST so all ctx fields are populated by the time the policy
    # factories run.
    # Local import to keep _context.py free of a circular dep with
    # _policies → _report → _context.
    from . import _policies
    _policies.maybe_register_from_params(_ctx)

    return _ctx


def get_ctx() -> Context:
    """Return the global context. Raises if ``context()`` was not called.

    Mostly internal — user code usually keeps the return value of
    ``context()`` directly. Exposed for SDK helpers that don't have a
    handle on it.
    """
    if _ctx is None:
        raise RuntimeError(
            "runq: context() must be called before using other runq APIs. "
            "Add `runq.context()` near the top of your script."
        )
    return _ctx


# ---- testing helper ----
#
# Not part of the public API but useful in tests to reset the singleton
# between test cases. Marked with underscore to discourage user calls.
def _reset_for_tests() -> None:
    global _ctx
    _ctx = None
