"""Python training harness for ``tests/l2/test_l2c_stage2_e2e.sh``.

End-to-end exercise of the SDK ↔ daemon contract:

1. ``runq.context()`` reads the daemon-injected ``RUNQ_*`` env.
2. ``shutil.disk_usage`` is monkey-patched at module load time to
   return "almost no free bytes" on the **first** call. This trips
   ``safe_save``'s disk-short branch on the first save attempt and
   causes the SDK to POST ``/api/internal/freeze-self`` to the
   daemon over the real unix socket.
3. After the daemon SIGSTOPs us, the HTTP POST blocks. The bash
   driver runs ``runq thaw``, the daemon SIGCONTs us, the POST
   returns, and the SDK loops back to retry the save.
4. The second call to disk_usage returns the real free bytes, so
   the retry succeeds; the task writes one metric event + one
   checkpoint event to jsonl and exits 0.

The bash driver verifies:
- the task transitioned through a frozen ("T") state
- one ``freeze-self`` HTTP arrived at the daemon
- the post-thaw jsonl contains both event types
- the task exited with status=succeeded
"""
from __future__ import annotations

import os
import shutil
import sys

import runq


_REAL_DISK_USAGE = shutil.disk_usage
_CALL_COUNT = {"n": 0}


def _staged_disk_usage(path):
    """Return ridiculous-low free bytes on call #1, real values after.

    Two distinct return values are intentional: the first one trips
    safe_save's freeze path; subsequent calls (including the post-thaw
    re-check) see real disk so the save completes.
    """
    _CALL_COUNT["n"] += 1
    if _CALL_COUNT["n"] == 1:
        return shutil._ntuple_diskusage(1 << 50, 0, 10)
    return _REAL_DISK_USAGE(path)


shutil.disk_usage = _staged_disk_usage  # type: ignore[assignment]


def _save_fn(path: str, obj):
    # Minimal save — no torch dep, so this harness runs anywhere.
    with open(path, "w") as f:
        f.write(str(obj))


def main() -> int:
    ctx = runq.context()
    if ctx.mode != "daemon":
        print(f"expected daemon mode, got {ctx.mode!r}", file=sys.stderr)
        return 2

    # Sentinel files the bash driver greps for to make assertions
    # before the task exits.
    started_marker = os.environ.get("RUNQ_E2E_STARTED_MARKER")
    if started_marker:
        with open(started_marker, "w") as f:
            f.write(f"task_id={ctx.task_id}\n")

    runq.report({"loss": 1.0}, step=0)
    runq.safe_save(
        "ckpt.pt",
        {"step": 0, "loss": 1.0},
        save_fn=_save_fn,
        size_hint=100,
        step=0,
    )
    runq.report({"loss": 0.5}, step=1)

    finished_marker = os.environ.get("RUNQ_E2E_FINISHED_MARKER")
    if finished_marker:
        with open(finished_marker, "w") as f:
            f.write(f"task_id={ctx.task_id}\ndisk_calls={_CALL_COUNT['n']}\n")

    print(f"task {ctx.task_id} finished after {_CALL_COUNT['n']} disk_usage calls")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
