"""Public SDK exception types.

All runq SDK exceptions inherit from :class:`RunqError` so user code can
catch ``except runq.RunqError`` to handle every SDK-originated failure
uniformly. Specific subclasses carry diagnostic fields so the user can
branch on them (e.g. "save failed because disk full" vs "save failed
because path was bad").
"""
from __future__ import annotations


class RunqError(Exception):
    """Base class for every exception the runq SDK raises.

    User code can ``except runq.RunqError`` to catch any SDK failure
    while letting unrelated exceptions (KeyboardInterrupt,
    ImportError, ...) propagate as usual.
    """


class RunqDiskFullError(RunqError):
    """Raised by ``safe_save`` in ``no_daemon`` mode when disk space is
    insufficient and the daemon isn't available to freeze the task.

    HPC users will typically see this once during a sweep, log it, and
    move on — the task is dead but the rest of the sweep keeps going.

    Attributes
    ----------
    mount : str
        Mountpoint that ran out.
    free_bytes : int
        How much was actually available at the time of the check.
    needed_bytes : int
        How much the save was estimated to need (incl. safety factor).
    """

    def __init__(
        self,
        message: str = "",
        *,
        mount: str = "",
        free_bytes: int = 0,
        needed_bytes: int = 0,
    ) -> None:
        if not message:
            gib = lambda b: b / (1 << 30)  # noqa: E731
            message = (
                f"runq.safe_save: disk full on {mount!r}: "
                f"{gib(free_bytes):.2f} GiB free, "
                f"{gib(needed_bytes):.2f} GiB needed"
            )
        super().__init__(message)
        self.mount = mount
        self.free_bytes = free_bytes
        self.needed_bytes = needed_bytes


class RunqEarlyStopSignal(RunqError):
    """Internal — surfaces from ``runq.loop()`` when ``@runq.early_stop``
    decides training should stop.

    Most user code shouldn't see this directly; ``runq.loop()`` catches
    it and breaks the iteration. Documented so power users who write
    their own iteration without ``runq.loop()`` know the type.
    """
