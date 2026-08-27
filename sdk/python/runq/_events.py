"""Event emission — the SDK's jsonl writers and the three-file routing.

Three-file contract (RQ2-1): each event stream has its own file so the
semantics split at the API boundary and downstream never disambiguates:

- ``metrics.jsonl`` — pure per-step metric stream (``report`` /
  ``log_metric``); unbounded, summarized on ingest (+ pyramid).
- ``events.jsonl`` — lifecycle plane: ``checkpoint`` / ``preempted`` /
  ``loop_break`` and future lifecycle types.
- ``results.jsonl`` — ``runq.record`` data plane; bounded facts,
  ingested in full (see :mod:`runq._record`).

JSONL contract for metric events (matches daemon-side reap):

    {"type":"metric","key":"loss","value":0.42,"step":100,"ts":1700000000}

``task_id`` / ``job_id`` are NOT written by the SDK. The daemon fills
those in on reap from its own task context. This keeps the SDK ignorant
of internal DB schemas and lets us rename store columns without
breaking users.

Step resolution
---------------
Step 6 retired the hidden monotonic auto-step counter. The single
source of truth for "what step is this?" is ``ctx.current_step``,
maintained by:

- ``report(metrics, step=N)`` — explicit step writes back to ctx.
- ``log_metric(..., step=N)`` — same; keeps the two helpers in sync.
- ``loop()`` (step 8) — sets it once per iteration.

When the caller omits ``step`` and ctx has never been written to,
``step=None`` lands in the jsonl event. Daemon-side reap tolerates that
(orders by ts as fallback).
"""
from __future__ import annotations

import json
import sys
import time
import warnings

from ._context import get_ctx
from ._prefix import apply_prefix


def _append_jsonl_line(path, obj: dict, kind: str) -> None:
    """Atomically append one JSON line to ``path``.

    Best-effort by design: if writing fails (read-only fs, dir gone),
    print a warning to stderr and continue. Crashing the user's training
    run because we couldn't log a number is the wrong trade.
    """
    if path is None:
        # Shouldn't happen post-context() — context() always sets the paths
        # (cwd in manual mode). Defensive guard; warn so users notice if
        # data is being silently discarded.
        warnings.warn(
            f"runq: {kind} file is not set — event discarded. "
            "Call runq.context() before logging.",
            stacklevel=4,
        )
        return
    try:
        # Append-and-flush per event. Slight overhead but guarantees
        # tail -f gives a live view in daemon mode (Codex review #1).
        with open(path, "a", encoding="utf-8") as f:
            f.write(json.dumps(obj, separators=(",", ":")) + "\n")
            f.flush()
    except OSError as e:
        print(
            f"runq: failed to append event to {path}: {e}",
            file=sys.stderr,
        )


def _append_event(event: dict) -> None:
    """Route one SDK event to its file by type (three-file contract).

    ``metric`` → metrics.jsonl; every other type (``checkpoint`` /
    ``preempted`` / ``loop_break`` and future lifecycle events) →
    events.jsonl. Result records never come through here — ``record``
    writes via :func:`_append_result`.
    """
    ctx = get_ctx()
    if event.get("type") == "metric":
        _append_jsonl_line(ctx.metrics_file, event, "metrics")
    else:
        _append_jsonl_line(ctx.events_file, event, "events")


def _append_result(record: dict) -> None:
    """Append one ``runq.record`` result record to results.jsonl."""
    ctx = get_ctx()
    _append_jsonl_line(ctx.results_file, record, "results")


def log_metric(key: str, value: float, step: int | None = None) -> None:
    """Append a single metric event to ``metrics.jsonl``.

    Low-level helper. For training loops, prefer :func:`runq.report`
    which handles multiple metrics + early-stop check in one call.

    Step semantics (step 6 contract)
    --------------------------------
    - ``step=N`` (explicit) — written to jsonl AND saved back to
      ``ctx.current_step``. Future ``report`` / ``log_metric`` calls
      without ``step=`` will see ``N`` until it's updated.
    - ``step=None`` — read ``ctx.current_step`` as fallback. If the ctx
      step has never been set, ``step=null`` ends up in jsonl. There is
      NO auto-increment counter.

    Why the write-back: it keeps log_metric (side-channel) consistent
    with report (canonical) so the two helpers can be interleaved
    without their step views drifting apart.

    ``step=0`` is a legitimate value; only ``None`` triggers the ctx
    fallback (Codex review #6 — never use ``step or X`` here).
    """
    ctx = get_ctx()
    if step is not None:
        ctx.current_step = step
        resolved_step: int | None = step
    else:
        resolved_step = ctx.current_step
    _append_event(
        {
            "type": "metric",
            "key": apply_prefix(key),
            "value": float(value),
            "step": resolved_step,
            "ts": int(time.time()),
        }
    )


# ---- testing helper ----
def _reset_for_tests() -> None:
    """No-op now that the auto-step counter is gone (step 6).

    Kept for conftest.py compatibility — removing the call sites would
    just create churn. Future per-module module-level state can hang
    off here.
    """
    return
