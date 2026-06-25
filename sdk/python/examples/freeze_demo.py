"""End-to-end demo of the runq Python SDK against a live runq daemon.

What this exercises
-------------------
- ``runq.context()`` — pick up daemon-injected env (RUNQ_TASK_ID,
  RUNQ_TASK_DIR, RUNQ_SOCKET_PATH, params).
- ``runq.loop(...)`` — drives the epoch counter and breaks on
  ``Decision.should_stop``. Optional tqdm progress bar if installed.
- ``runq.report(...)`` — canonical metric write. Step is omitted at
  call sites so loop()'s ``ctx.current_step`` is used (F5.5).
- ``@runq.early_stop`` — user-defined stopping rule. Returning a
  string makes that string the recorded reason.
- ``@runq.safe_save(keep_last_n=3, keep_best=True)`` — parameterized
  decorator: TOCTOU-safe save + manifest-scoped rotation. Disk-full
  mid-save would trigger a freeze-self HTTP to the daemon and resume
  after thaw. Demonstrating freeze itself requires actual disk
  pressure or a controlled environment; the smoke test in
  ``test_examples.py`` covers the happy path.
- ``ctx.wandb_like_cfg`` / ``ctx.wandb_like_metric`` — user-owned
  wandb wiring (commented out so the demo doesn't pull wandb in).

How to actually run
-------------------
1. Start the lab daemon:

       runqd &

2. Submit this script (assumes a job yaml that points here):

       runq submit examples/freeze_demo_job.yaml

The daemon injects the RUNQ_* env vars and wires up the unix socket
for freeze coordination. See ``demo/l2c/stage2_sdk_design.md`` for
the env contract.

Notes for the lazy reader
-------------------------
- ``runq.loop()`` already sets the default ``step`` on each iter, so
  ``runq.report({...})`` without ``step=`` works.
- ``runq.safe_save(...)`` likewise picks up the loop's step; the
  ``is_best=`` flag drives manifest's single-best invariant +
  ``keep_best=True`` rescue.
- ``@runq.early_stop`` is the explicit escape hatch — for canned
  policies, use ``runq.patience(...)`` / ``runq.threshold(...)`` /
  ``runq.convergence(...)`` instead.
"""
from __future__ import annotations

import math

import torch
import torch.nn as nn

import runq


@runq.safe_save(keep_last_n=3, keep_best=True)
def save_checkpoint(path, model, optim):
    """User-defined checkpoint writer.

    Runs inside the TOCTOU + freeze flow when called. The SDK strips
    its managed kwargs (step / is_best / size_hint) before calling
    this function — user code stays clean.
    """
    torch.save(
        {"model": model.state_dict(), "optim": optim.state_dict()},
        path,
    )


def train() -> float:
    ctx = runq.context()

    # OPTIONAL — user-driven wandb mirror. SDK never imports wandb.
    #   import wandb
    #   wandb.init(**ctx.wandb_like_cfg)
    # ... and inside the loop, after report():
    #   wandb.log({**ctx.wandb_like_metric, "lr": ctx.get("lr", 0.01)},
    #             step=ctx.current_step)

    seed = ctx.get("seed", 0)
    lr = ctx.get("lr", 0.01)
    epochs = ctx.get("epochs", 50)

    torch.manual_seed(seed)
    model = nn.Sequential(nn.Linear(8, 32), nn.ReLU(), nn.Linear(32, 1))
    optim = torch.optim.SGD(model.parameters(), lr=lr)
    X = torch.randn(256, 8)
    y = torch.randn(256, 1)

    # Stop early if val_loss drops below a (toy) convergence threshold.
    @runq.early_stop
    def converged(history, current):
        if current.get("val_loss", math.inf) < 1e-3:
            return f"converged at step {ctx.current_step}"
        return False

    best_loss = math.inf
    for _ in runq.range(epochs):
        optim.zero_grad()
        loss = ((model(X) - y) ** 2).mean()
        loss.backward()
        optim.step()

        val_loss = loss.item()
        is_best = val_loss < best_loss
        if is_best:
            best_loss = val_loss

        # Canonical metric write — drives the @early_stop hook,
        # updates ctx.current_step, appends to metrics.jsonl.
        runq.report({"val_loss": val_loss})

        # Disk-safe save with manifest-scoped rotation. The filename
        # must include a varying component (here, the step number) so
        # that keep_last_n=3 can actually retain 3 distinct files.
        # A fixed name like "ckpt.pt" would be overwritten in place
        # every epoch, making rotation meaningless.
        # ``size_hint`` is auto-estimated from the model + optim dicts.
        save_checkpoint(
            f"ckpt-{ctx.current_step}.pt", model, optim, is_best=is_best,
        )

    return best_loss


def main() -> int:
    best = train()
    print(f"training finished, best val_loss={best:.6f}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
