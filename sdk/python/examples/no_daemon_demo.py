"""``no_daemon`` mode demo — HPC-style use of runq, no freeze coordination.

This is the same SDK code as ``freeze_demo.py`` but the task runs
without a reachable daemon. The contract change is **only on
safe_save**: instead of POSTing freeze-self to the daemon, a disk-
short path raises :class:`runq.RunqDiskFullError` and the task dies.
Everything else (jsonl writes, params, early_stop, loop, report)
works exactly the same.

HPC researchers run their tasks under this mode because the cluster
admin doesn't let runqd run on login / compute nodes (see the
"Deployment topology" section of ``stage2_sdk_design.md``). They
accept that disk-full = task dies, because in HPC the user *should*
be on a data disk with quota they understand.

How to run
----------
::

    export RUNQ_NO_DAEMON=1
    export RUNQ_TASK_ID=demo-$(date +%s)
    export RUNQ_TASK_DIR=/tmp/runq-demo
    export RUNQ_METRICS_FILE=/tmp/runq-demo/metrics.jsonl
    export RUNQ_CHECKPOINT_DIR=/tmp/runq-demo/ckpts
    mkdir -p /tmp/runq-demo
    python examples/no_daemon_demo.py

The script intentionally triggers ``RunqDiskFullError`` via an absurd
``size_hint`` to show the failure path. Real HPC code would just
let it propagate (or catch and bail with a clear message).
"""
from __future__ import annotations

import sys

import torch
import torch.nn as nn

import runq


def train() -> int:
    """Returns process exit code: 0 happy, 1 disk-full (expected here)."""
    ctx = runq.context()
    if ctx.mode != "no_daemon":
        print(
            f"this demo expects no_daemon mode (got {ctx.mode!r}); "
            "set RUNQ_NO_DAEMON=1 and re-run",
            file=sys.stderr,
        )
        return 2

    torch.manual_seed(0)
    model = nn.Sequential(nn.Linear(4, 8), nn.Linear(8, 1))
    X = torch.randn(64, 4)
    y = torch.randn(64, 1)

    for _ in runq.range(5):
        loss = ((model(X) - y) ** 2).mean().item()

        runq.report({"val_loss": loss})

        try:
            # An impossible size_hint forces the disk-short branch and
            # demonstrates the no_daemon contract: raise instead of
            # freeze. In real code you'd let safe_save estimate.
            runq.safe_save(
                f"ckpt-{ctx.current_step}.pt",
                model.state_dict(),
                size_hint=10 ** 18,
            )
        except runq.RunqDiskFullError as e:
            print(
                f"hit disk-full path: mount={e.mount!r}, "
                f"free_bytes={e.free_bytes}, needed_bytes={e.needed_bytes}",
                file=sys.stderr,
            )
            print(
                "HPC task would now exit (or fall back to a cheaper "
                "checkpoint); freeze is OFF in no_daemon mode.",
                file=sys.stderr,
            )
            return 1
    return 0


if __name__ == "__main__":
    sys.exit(train())
