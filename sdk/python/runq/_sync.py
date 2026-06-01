"""Ephemeral shell-out to ``runq sync`` for jsonl → DB ingestion.

See F10 in ``demo/l2c/stage2_sdk_design.md``. This module is just a
wrapper around the Go-side ``runq sync`` CLI; the SDK never
reimplements the ingest logic.

Why a wrapper at all
--------------------
β-mode in F10: users who want a live dashboard during long training
runs can call ``runq.sync_now()`` after each epoch / report cycle.
This triggers the same ingest path the lab daemon runs at task exit.
~50ms per call, not a hot path.

α-mode (HPC template ``trap ... EXIT``) doesn't need this module —
that path is pure shell driven by the future ``submit.sh.template``.

Failure policy
--------------
**Never raises**. Sync is best-effort convenience, not a critical
training path. Return ``True`` on subprocess exit 0, ``False`` on any
failure (binary missing, subprocess error, timeout, sqlite contention,
manual mode with no task_dir, ...). The next call retries; α-mode
task-exit ingest will catch up at the end anyway.

Failures are silent on stdout/stderr; we may optionally log at DEBUG
level so power users tailing the SDK logs can see why sync is silently
no-op'ing, but never escalate beyond that.
"""
from __future__ import annotations

import logging
import subprocess

from ._context import get_ctx

_LOG = logging.getLogger(__name__)

# Default subprocess timeout in seconds. 5s is generous for the size of
# jsonl files a single task realistically produces between epoch reports;
# anything longer means the sqlite/FS is wedged and the user's training
# loop shouldn't wait synchronously.
_DEFAULT_TIMEOUT_S = 5.0

# Name of the Go CLI binary on PATH. Configurable here so the test
# suite (or a dev install) can swap to a different name without
# rewriting call sites.
_BINARY_NAME = "runq"


def _build_argv(task_dir: str) -> list[str]:
    """Construct the argv list for the ``runq sync`` invocation.

    Split out of :func:`sync_now` so tests can assert exactly what gets
    shelled out without mocking subprocess.run's signature.
    """
    return [_BINARY_NAME, "sync", "--task-dir", str(task_dir)]


def sync_now(*, timeout_s: float = _DEFAULT_TIMEOUT_S) -> bool:
    """Trigger a one-shot reap of this task's jsonl into the job DB.

    Subprocess to ``runq sync --task-dir=<ctx.task_dir>``. Best-effort:
    returns ``True`` on subprocess exit 0, ``False`` on any failure
    (binary missing, sqlite locked, timeout, etc.). **Never raises** —
    sync is a convenience, not a critical path.

    Parameters
    ----------
    timeout_s :
        Subprocess timeout in seconds. Default ~5s. Pass a smaller
        value when calling per-batch (rather than per-epoch) so the
        training loop isn't held hostage by a stuck sqlite.

    Returns
    -------
    bool
        ``True`` if subprocess exited 0. ``False`` for all other
        outcomes (including manual mode + binary-missing + timeout).
    """
    ctx = get_ctx()
    if ctx.task_dir is None:
        return False
    args = _build_argv(str(ctx.task_dir))
    try:
        result = subprocess.run(
            args=args,
            timeout=timeout_s,
            check=False,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.PIPE,
            text=True
        )
        if result.returncode == 0:
            return True
        _LOG.debug(f"Sync failed with return code {result.returncode}. Stderr: {result.stderr}")
        return False
    except (FileNotFoundError, PermissionError, subprocess.TimeoutExpired, OSError) as e:
        _LOG.debug(f"Sync suppressed exception: {e}")
        return False

