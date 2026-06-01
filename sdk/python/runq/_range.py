"""Shared iterator core for ``runq.range()`` and ``runq.loop()``.

Both iterators share:
- SIGTERM preemption (flag-based, cooperative)
- early-stop check via ``ctx._last_decision``
- stale decision reset at start

``range()`` is the numeric step owner; ``loop()`` wraps arbitrary iterables
and uses enumerate index as step.
"""

from __future__ import annotations

import signal
import threading
import time as _time
from typing import Any, Generator

_preempted = False
_prev_handler = None
_handler_installed = False
_lock = threading.Lock()


def _install_signal_handler() -> None:
    """Install SIGTERM handler once, main thread only."""
    global _handler_installed, _prev_handler
    if _handler_installed:
        return
    with _lock:
        if _handler_installed:
            return
        try:
            _prev_handler = signal.getsignal(signal.SIGTERM)

            def _handler(signum, frame):
                global _preempted
                _preempted = True
                if callable(_prev_handler) and _prev_handler not in (
                    signal.SIG_DFL,
                    signal.SIG_IGN,
                ):
                    _prev_handler(signum, frame)

            signal.signal(signal.SIGTERM, _handler)
            _handler_installed = True
        except (OSError, ValueError):
            pass


def is_preempted() -> bool:
    """Return True if a SIGTERM has been received."""
    return _preempted


# ---- shared iterator core ----

def _check_break(ctx: Any, name: str, step_value: int | None) -> str | None:
    """Check preemption + early-stop. Returns event type string or None.

    Called before each yield by both range() and loop(). If a break
    condition is met, emits the appropriate jsonl event and returns
    the event type ("preempted" or "loop_break"). Caller should
    ``return`` from the generator on non-None.
    """
    from ._events import _append_event

    if _preempted:
        _append_event({
            "type": "preempted",
            "step": step_value,
            "ts": int(_time.time()),
        })
        return "preempted"

    if ctx is not None:
        decision = ctx._last_decision
        if decision is not None and getattr(decision, "should_stop", False):
            _append_event({
                "type": "loop_break",
                "name": name,
                "reason": getattr(decision, "reason", None),
                "ts": int(_time.time()),
            })
            return "loop_break"

    return None


def _init_iterator(ctx: Any) -> None:
    """Shared setup at the start of range() and loop().

    Installs SIGTERM handler and resets stale early-stop decisions.
    """
    _install_signal_handler()
    if ctx is not None:
        ctx._last_decision = None


# ---- range() ----

def range(start: int, stop: int | None = None, step: int = 1) -> Generator[int, None, None]:
    """Training-loop iterator with auto step tracking and preemption.

    Signature mirrors built-in ``range()``::

        runq.range(10)          # 0..9
        runq.range(5, 10)       # 5..9
        runq.range(0, 100, 10)  # 0, 10, 20, ..., 90

    Before each yield:
    - checks the preemption flag (SIGTERM received → stop)
    - checks ``ctx._last_decision`` from ``runq.report()`` (early-stop → stop)
    - sets ``ctx.current_step = i``
    """
    from ._context import get_ctx

    try:
        ctx = get_ctx()
    except RuntimeError:
        ctx = None

    _init_iterator(ctx)

    # Match built-in range() signature
    if stop is None:
        start, stop = 0, start

    if step == 0:
        raise ValueError("runq.range() arg 3 must not be zero")

    i = start
    while (step > 0 and i < stop) or (step < 0 and i > stop):
        if _check_break(ctx, "range", i) is not None:
            return

        if ctx is not None:
            ctx.current_step = i

        yield i
        i += step


def _reset_for_tests() -> None:
    """Reset module state for test isolation."""
    global _preempted, _handler_installed, _prev_handler
    _preempted = False
    _handler_installed = False
    _prev_handler = None
