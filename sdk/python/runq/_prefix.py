"""Per-call-stack metric-key prefix support — backing for ``@runq.log_group``.

Why a separate module
---------------------
``_events`` (log_metric) and ``_report`` (report) both need to read the
current prefix when assembling jsonl events. ``_loop`` defines
``log_group`` which writes the prefix. Putting the state in either
caller would create a cycle (``_events`` ↔ ``_loop``). A tiny standalone
module avoids the headache and keeps the responsibility crisp.

Stack-of-strings semantics
--------------------------
The prefix is a *stack* (tuple), not a single string, so nesting works
naturally::

    with runq.log_group("train"):
        with runq.log_group("step1"):
            runq.log_metric("loss", l)   # key becomes "train/step1/loss"

Each ``log_group`` pushes one segment; the joined prefix is what
``apply_prefix`` returns. ``contextvars.ContextVar`` gives us push/pop
semantics that survive thread + asyncio boundaries (each task / thread
sees its own stack).
"""
from __future__ import annotations

import contextvars

# Stack of active prefix segments. Empty tuple means "no active group".
# Tuple (immutable) is the right type for ContextVar values — copying is
# implicit when the runtime forks state for a new task / thread, and
# nobody can accidentally mutate the previous frame's stack.
_prefix_stack: contextvars.ContextVar[tuple[str, ...]] = contextvars.ContextVar(
    "runq_log_group_stack", default=()
)


def current_prefix() -> str:
    """Return the joined prefix string, or '' if no group is active.

    >>> # inside `with log_group("train"): with log_group("loss"): ...`
    >>> current_prefix()
    'train/loss'
    """
    stack = _prefix_stack.get()
    return "/".join(stack) if stack else ""


def apply_prefix(key: str) -> str:
    """Prepend the current group prefix to ``key``.

    >>> apply_prefix("loss")   # no group active
    'loss'
    >>> # inside `with log_group("train"):`
    >>> apply_prefix("loss")
    'train/loss'
    """
    p = current_prefix()
    return f"{p}/{key}" if p else key
