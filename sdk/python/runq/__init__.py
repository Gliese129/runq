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
from ._loop import epoch, log_group, loop
from ._policies import convergence, patience, threshold
from ._report import Decision, early_stop, report
from ._safe_save import safe_save
from ._sync import sync_now
from ._transport import TransportError

# NB: __all__ is grouped by *topic* (init / metrics / policies / loop /
# exceptions), not alphabetically. The grouping makes the API surface
# discoverable to lazy users reading ``help(runq)``; ruff's RUF022 wants
# alphabetical, which would scramble it. Hence the noqa.
__all__ = [  # noqa: RUF022
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
    # Training-loop ergonomics (step 8).
    "loop",
    "epoch",
    "log_group",
    "safe_save",
    # Ephemeral reap (F10 β-mode).
    "sync_now",
    # Exceptions.
    "RunqError",
    "RunqDiskFullError",
    "RunqEarlyStopSignal",
    "TransportError",
]

__version__ = "0.1.0"
