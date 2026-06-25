"""runq — Lab GPU scheduler SDK (in-task client).

Public API::

    import runq

    ctx = runq.context()

    # Typed params with auto merge from sweep
    @runq.dataclass(auto_overwrite=True)
    class Params:
        lr: float = 0.001
        batch_size: int = 32
    cfg = Params()

    # Training loop with auto step + preemption
    for step in runq.range(100):
        loss = train(model)
        runq.log_metric("loss", loss)             # step auto-populated
        runq.report({"val_loss": evaluate(model)}) # early-stop check
        runq.safe_save("ckpt.pt", model.state_dict())

    # Resume from latest checkpoint
    ckpt = runq.latest_checkpoint()
"""

from ._config import dataclass
from ._context import Context, ParamDict, context, get_ctx
from ._events import log_metric
from ._exceptions import RunqDiskFullError, RunqEarlyStopSignal, RunqError
from ._loop import log_group, loop
from ._manifest import best_checkpoint, latest_checkpoint
from ._policies import convergence, patience, threshold
from ._range import is_preempted, range
from ._report import Decision, early_stop, report
from ._safe_save import safe_save
from ._transport import TransportError

__all__ = [  # noqa: RUF022
    # Init + context
    "Context",
    "ParamDict",
    "context",
    "get_ctx",
    # Typed parameter dataclass
    "dataclass",
    # Training loop
    "loop",
    "range",
    "is_preempted",
    # Metrics
    "log_metric",
    "log_group",
    # Early stop
    "report",
    "early_stop",
    "Decision",
    "patience",
    "threshold",
    "convergence",
    # Checkpoint
    "safe_save",
    "best_checkpoint",
    "latest_checkpoint",
    # Exceptions
    "RunqError",
    "RunqDiskFullError",
    "RunqEarlyStopSignal",
    "TransportError",
]

__version__ = "0.2.0"


def __getattr__(name: str):
    """Module-level attribute access for convenience properties."""
    if name == "preempted":
        from ._range import is_preempted
        return is_preempted()
    if name == "seed":
        from ._context import get_ctx
        return get_ctx().seed
    if name == "params":
        from ._context import get_ctx
        return get_ctx().params
    raise AttributeError(f"module 'runq' has no attribute {name!r}")
