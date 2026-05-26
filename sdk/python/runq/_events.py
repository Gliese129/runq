"""Metric / event emission to metrics.jsonl.

JSONL contract (matches what the daemon's ``ReapTaskOutputs`` expects):

    {"type":"metric","key":"loss","value":0.42,"step":100,"ts":1700000000}

``task_id`` / ``job_id`` are NOT written by the SDK. The daemon fills
those in on reap from its own task context. This keeps the SDK ignorant
of internal DB schemas and lets us rename store columns without
breaking users.
"""
from __future__ import annotations

import json
import sys
import time
from typing import Any, Optional

from ._context import get_ctx

# Module-level monotonic counter used when the caller omits step.
# Reset is provided for tests only — see _reset_for_tests().
_auto_step = 0


def _next_auto_step() -> int:
    global _auto_step
    _auto_step += 1
    return _auto_step


def _append_event(event: dict) -> None:
    """Atomically append one JSON line to the metrics file.

    Best-effort by design: if writing fails (read-only fs, dir gone),
    print a warning to stderr and continue. Crashing the user's training
    run because we couldn't log a number is the wrong trade.
    """
    ctx = get_ctx()
    if ctx.metrics_file is None:
        # Shouldn't happen post-context() — context() always sets a path
        # (cwd in manual mode). Defensive guard.
        return
    try:
        # Append-and-flush per event. Slight overhead but guarantees
        # tail -f gives a live view in daemon mode (Codex review #1).
        with open(ctx.metrics_file, "a", encoding="utf-8") as f:
            f.write(json.dumps(event, separators=(",", ":")) + "\n")
            f.flush()
    except OSError as e:
        print(
            f"runq: failed to append event to {ctx.metrics_file}: {e}",
            file=sys.stderr,
        )


def log_metric(key: str, value: float, step: Optional[int] = None) -> None:
    """Append a single metric event to ``metrics.jsonl``.

    Low-level helper. For training loops, prefer :func:`runq.report`
    which handles multiple metrics + early-stop check in one call.

    ``step=0`` is a legitimate value; only ``None`` triggers SDK
    auto-assignment (Codex review #6 — never use ``step or X`` here).
    """
    resolved_step = step if step is not None else _next_auto_step()
    _append_event(
        {
            "type": "metric",
            "key": key,
            "value": float(value),
            "step": resolved_step,
            "ts": int(time.time()),
        }
    )


# ---- testing helper ----
def _reset_for_tests() -> None:
    """Reset the auto-step counter. Tests only."""
    global _auto_step
    _auto_step = 0
