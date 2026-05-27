"""runq — Lab GPU scheduler SDK (in-task client).

Public API, lazy user path:

    import runq

    ctx = runq.context()
    runq.log_metric("loss", 0.42, step=epoch)     # low-level helper
    runq.safe_save("ckpt.pt", model.state_dict(), # disk-safe save
                   step=epoch, is_best=True, size_hint=...)

Stage 2 will add: runq.report, decorator-form safe_save with size
auto-estimation, runq.loop, @runq.early_stop, @runq.epoch.

See `demo/l2c/stage2_sdk_design.md` for the full API design and
behavior contracts.
"""

from ._context import Context, context, get_ctx
from ._events import log_metric
from ._exceptions import RunqDiskFullError, RunqEarlyStopSignal, RunqError
from ._policies import convergence, patience, threshold
from ._report import Decision, early_stop, report
from ._safe_save import safe_save
from ._transport import TransportError

__all__ = [
    "Context",
    "context",
    "get_ctx",
    "log_metric",
    "report",
    "early_stop",
    "Decision",
    # Built-in early-stop policy factories (step 7).
    "patience",
    "threshold",
    "convergence",
    "safe_save",
    # Exceptions.
    "RunqError",
    "RunqDiskFullError",
    "RunqEarlyStopSignal",
    "TransportError",
]

__version__ = "0.1.0"
