"""Training-loop ergonomics — ``runq.loop`` / ``@runq.epoch`` / ``@runq.log_group``.

Three small surfaces packaged together because they all live near the
training loop in user code:

1. ``loop(iterable)`` — generator that yields items and short-circuits
   on preemption (SIGTERM) or early-stop from ``report()``.
2. ``@epoch`` — timing-only decorator that emits one
   ``epoch_time_seconds`` event per call. Does NOT touch step (Codex
   review #7 — step ownership belongs to ``report`` / ``safe_save``).
3. ``log_group(prefix)`` — dual context manager / decorator that
   prefixes subsequent metric keys.

Coupling boundaries
-------------------
- ``loop`` uses the shared iterator core in ``_range`` (``_check_break``
  + ``_init_iterator``) — same preemption + early-stop as ``range()``.
- ``epoch`` writes via ``_append_event`` and respects the active
  ``log_group`` prefix (uses :func:`runq._prefix.apply_prefix`).
- ``log_group`` writes to ``runq._prefix._prefix_stack`` ContextVar.

The prefix ContextVar lives in :mod:`runq._prefix` to dodge the
``_events`` ↔ ``_loop`` circular import.
"""
from __future__ import annotations

import functools
import time
from collections.abc import Callable, Iterable, Iterator
from typing import Any

from ._context import get_ctx
from ._events import _append_event
from ._prefix import _prefix_stack, apply_prefix

# ---- loop() -------------------------------------------------------

def loop(
    iterable: Iterable[Any],
    *,
    name: str | None = None,
    total: int | None = None,
) -> Iterator[Any]:
    """Generator wrapping ``iterable`` with preemption + early-stop.

    Shares the iterator core with :func:`runq.range`:

    - SIGTERM preemption (cooperative flag)
    - early-stop via ``ctx._last_decision`` (set by ``report()``)
    - stale decision reset at start

    ``loop()`` wraps arbitrary iterables (dataloaders, custom sequences);
    ``range()`` is the numeric step owner. Use ``loop()`` when your
    iterator is not a simple int range.

    Step ownership
    --------------
    ``loop`` uses the enumerate index as ``ctx.current_step``.
    Subsequent ``runq.report`` / ``runq.safe_save`` calls without an
    explicit ``step=`` use that value.

    Parameters
    ----------
    iterable :
        Any iterable (dataloader, list, generator, …).
    name :
        Description for the progress bar AND the ``loop_break`` event
        if early stop fires.
    total :
        Forwarded to tqdm when iterable has no ``__len__``.

    Yields
    ------
    Items from ``iterable``, until exhausted, preempted, or early-stopped.
    """
    from ._range import _check_break, _init_iterator

    try:
        ctx = get_ctx()
    except RuntimeError:
        ctx = None

    _init_iterator(ctx)

    iterator = _maybe_tqdm(iterable, name=name, total=total)

    for i, item in enumerate(iterator):
        if _check_break(ctx, name or "loop", i) is not None:
            return

        if ctx is not None:
            ctx.current_step = i

        yield item


def _maybe_tqdm(iterable, *, name, total):
    """Wrap iterable in tqdm if available, else return as-is.

    tqdm is a soft dependency. Importing it lazily here keeps SDK
    startup cheap and lets users skip the install entirely.
    """
    try:
        from tqdm import tqdm  # type: ignore
    except ImportError:
        return iter(iterable)
    return tqdm(iterable, desc=name, total=total, leave=False)


# ---- @epoch -------------------------------------------------------

def epoch(fn: Callable[..., Any]) -> Callable[..., Any]:
    """Timing decorator — emits one ``epoch_time_seconds`` metric event.

    Pure ergonomics. Does NOT:
    - assign or advance step (Codex review #7)
    - interact with early-stop hooks / history
    - know anything about your training loop structure

    Composes cleanly with ``@log_group``::

        @runq.log_group("train")
        @runq.epoch
        def train_one_epoch(...):
            ...
        # → key becomes "train/epoch_time_seconds"

    The decorator emits the metric event inside a ``try/finally`` so
    a raising user fn still records its wall-clock (the fact that the
    epoch crashed is captured by traceback; the timing might be
    useful for "did it OOM after 4 minutes or 4 hours" debugging).
    """
    @functools.wraps(fn)
    def wrapper(*args, **kwargs):
        t0 = time.time()
        try:
            return fn(*args, **kwargs)
        finally:
            elapsed = time.time() - t0
            _append_event(
                {
                    "type": "metric",
                    "key": apply_prefix("epoch_time_seconds"),
                    "value": float(elapsed),
                    # Pure timing — no step concept; jsonl gets null.
                    "step": None,
                    "ts": int(time.time()),
                }
            )

    return wrapper


# ---- @log_group ---------------------------------------------------

def log_group(prefix: str):
    """Prefix all subsequent metric keys with ``prefix`` until exit.

    Dual-form API — usable BOTH as a context manager and as a
    decorator. The two forms behave identically; pick whichever reads
    cleaner at the call site::

        # Context manager
        with runq.log_group("train"):
            runq.log_metric("loss", l)   # key → "train/loss"

        # Decorator
        @runq.log_group("train")
        def train_one_epoch():
            runq.log_metric("loss", l)   # key → "train/loss"

    Nesting composes — inner prefixes append with ``/`` separator::

        with runq.log_group("train"):
            with runq.log_group("step1"):
                runq.log_metric("loss", l)   # → "train/step1/loss"

    Validation
    ----------
    ``prefix`` must be a non-empty string with no ``/`` characters.
    The separator is reserved so users don't accidentally write
    ``log_group("a/b")`` and shadow the nesting story.

    Thread / asyncio safety
    -----------------------
    Backed by :data:`runq._prefix._prefix_stack`, a ``ContextVar``.
    Each thread / asyncio task sees its own stack; concurrent
    ``log_group`` blocks across tasks don't bleed prefixes into each
    other.

    Implementation
    --------------
    Backed by :class:`_LogGroup`, returned fresh per ``log_group(prefix)``
    call. The class implements ``__enter__`` / ``__exit__`` /
    ``__call__`` against ``_prefix_stack`` via ``ContextVar.set`` +
    ``ContextVar.reset`` tokens. The decorator path constructs a fresh
    ``_LogGroup`` per invocation so concurrent calls don't clobber
    each other's tokens.
    """
    if not isinstance(prefix, str):
        raise TypeError(
            f"runq.log_group: prefix must be a string, got {type(prefix).__name__}"
        )
    if not prefix:
        raise ValueError("runq.log_group: prefix must be non-empty")
    if "/" in prefix:
        raise ValueError(
            "runq.log_group: prefix must not contain '/' "
            "(use nested log_group() calls for hierarchical keys)"
        )
    return _LogGroup(prefix)


class _LogGroup:
    def __init__(self, prefix):
        self._prefix = prefix
        self._token = None
    def __enter__(self):
        cur = _prefix_stack.get()
        self._token = _prefix_stack.set((*cur, self._prefix))
        return self
    def __exit__(self, exc_type, exc, tb):
        _prefix_stack.reset(self._token)
        return False
    def __call__(self, fn):
        pfx = self._prefix
        @functools.wraps(fn)
        def wrapper(*args, **kwargs):
            # Fresh CM per call so concurrent invocations don't
            # share a single _token slot.
            with _LogGroup(pfx):
                return fn(*args, **kwargs)
        return wrapper
