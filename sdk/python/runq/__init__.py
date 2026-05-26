"""runq — Lab GPU scheduler SDK (in-task client).

Public API, lazy user path:

    import runq

    ctx = runq.context()                          # one-time init
    runq.log_metric("loss", 0.42, step=epoch)     # low-level helper

Stage 2 will add: runq.report, runq.safe_save, runq.loop,
@runq.early_stop, @runq.epoch, @runq.log_group.

See `demo/l2c/stage2_sdk_design.md` for the full API design and
behavior contracts.
"""

from ._context import Context, context, get_ctx
from ._events import log_metric

__all__ = [
    "Context",
    "context",
    "get_ctx",
    "log_metric",
]

__version__ = "0.1.0"
