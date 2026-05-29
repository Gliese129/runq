"""Canonical metric reporting + early-stop hook framework.

``runq.report`` is the lazy-user-facing call for "I finished a
step / epoch, here are my metrics, tell me if I should stop". It
writes the metrics to jsonl, updates the report history, runs any
``@runq.early_stop``-registered hooks, and hands back a Decision.

Why a separate module from log_metric
-------------------------------------
``log_metric`` is intentionally minimal (one metric, one row,
side-channel). ``report`` is the "training loop" call: multiple
metrics in one shot, history retention, early-stop hook fan-out. Same
jsonl format (one event per (key, value)), different ergonomics +
state.

Early-stop hooks
----------------
Users register stopping criteria as decorated functions:

    @runq.early_stop
    def plateau(history, metrics):
        # history: list of {"metrics": dict, "step": int|None, "ts": int}
        # metrics: dict just passed to report(...)
        # Return False / None to continue.
        # Return True for a generic stop ("user early_stop").
        # Return str to stop with that string as the reason.
        ...

Hooks are stored at module level. Every ``report`` call iterates them
in registration order; the **first truthy return wins** (short-circuit).
That keeps hook composition predictable — you don't need to think
about whether two hooks would "conflict", and side-effecting hooks
won't double-fire after a stop has been decided.

Decision contract
-----------------
``Decision`` is a frozen dataclass. Future extensions (pending,
suggestions) get added as optional fields without breaking unpack
patterns. We keep step 6 minimal (should_stop + reason) and add
fields lazily.
"""
from __future__ import annotations

import logging
import time
from collections.abc import Callable
from dataclasses import dataclass

from ._context import get_ctx
from ._events import _append_event
from ._prefix import apply_prefix

_LOG = logging.getLogger(__name__)


# Attribute name set on hooks that came from YAML auto-registration
# (see :mod:`runq._policies`). When the user writes ``@runq.early_stop``
# we look for hooks carrying this marker and evict them with a WARN —
# the contract is "code wins, yaml is a default".
_YAML_POLICY_MARK = "_runq_yaml_policy"


# ---- public types -------------------------------------------------

@dataclass(frozen=True)
class Decision:
    """What ``report`` tells you to do next.

    Frozen on purpose: hands of decisions get passed around, sometimes
    cached for analysis, sometimes serialized. Immutability removes a
    whole class of "someone mutated my Decision" bugs.

    Step 6 fields (more may land in later steps):
    - ``should_stop``: True iff some early-stop hook said so.
    - ``reason``: hook-supplied string (or ``"user early_stop"`` for a
      hook that returned bare ``True``). ``None`` when ``should_stop``
      is False.
    """

    should_stop: bool = False
    reason: str | None = None


# Hook signature: (history, metrics) -> truthy stops, falsy continues.
# Concrete: ``Callable[[list[dict], dict], Union[bool, str, None]]``.
EarlyStopHook = Callable[[list[dict], dict], bool | str | None]


# ---- module state -------------------------------------------------

# Registered hooks, in declaration order. The very first one that
# returns truthy wins.
_hooks: list[EarlyStopHook] = []

# History of (metrics, step, ts) entries handed to ``report``. Hooks
# see this as their first argument so they can compute moving averages
# / plateau detection / convergence checks. Reset between tests.
_history: list[dict] = []


# ---- public API ---------------------------------------------------

def early_stop(fn: EarlyStopHook) -> EarlyStopHook:
    """Register ``fn`` as an early-stop hook.

    Idempotent on the same function object: registering the same fn
    twice is a no-op (avoids double-fire when modules get re-imported
    in notebooks / interactive shells).

    YAML override (step 7)
    ----------------------
    If a YAML-auto-registered policy (see :mod:`runq._policies`)
    already lives in the hook list, the **first** user ``@early_stop``
    call removes it and emits a WARN. Reason: two sources of truth for
    "when to stop" is a debugging hell. Decorator wins, YAML is a
    default.

    Subsequent ``@early_stop`` decorators do not warn again — the YAML
    eviction only happens once per task.

    The function itself is returned unchanged — it can still be
    called directly for unit testing.

    Hook signature
    --------------
    Two positional args:

    1. ``history``: list of dicts ``{"metrics": ..., "step": ..., "ts": ...}``,
       one entry per prior ``report`` call within this task.
    2. ``metrics``: the current dict (already appended to history when
       the hook runs).

    Return values:

    - ``None`` / ``False`` → keep training.
    - ``True`` → stop, reason defaults to ``"user early_stop"``.
    - ``str`` → stop, with the string as the reason.
    """
    # Step 7: evict any YAML-registered policies on first user @early_stop.
    yaml_hooks = [h for h in _hooks if getattr(h, _YAML_POLICY_MARK, False)]
    if yaml_hooks:
        names = ", ".join(
            getattr(h, "_runq_yaml_policy_name", "?") for h in yaml_hooks
        )
        _LOG.warning(
            "runq: @runq.early_stop overrides YAML early_stopping policy (%s); "
            "removing YAML policy from hook chain",
            names,
        )
        for h in yaml_hooks:
            _hooks.remove(h)
    if fn not in _hooks:
        _hooks.append(fn)
    return fn


def report(
    metrics: dict,
    step: int | None = None,
) -> Decision:
    """Canonical "I just finished a step" call.

    Behavior in order:

    1. Resolve step: explicit ``step=N`` writes back to
       ``ctx.current_step`` and is used for this report. Otherwise
       the ctx value is used (may be None).
    2. Emit one ``{"type":"metric", ...}`` event per ``(key, value)``
       in ``metrics`` to jsonl. Same path / format as
       :func:`log_metric`.
    3. Append a single history entry — combined metrics dict + step +
       ts — so hooks see the whole picture.
    4. Run registered hooks via :func:`_run_early_stop_hooks`. First
       truthy return wins; the rest don't fire.
    5. Return ``Decision(should_stop, reason)``.

    Empty metrics dict is allowed — it's a "just poll the hooks"
    invocation. Useful when a hook needs to see ts/step progression
    even without a numeric metric.

    Parameters
    ----------
    metrics :
        Mapping of metric name → numeric value. Values are coerced to
        float for the jsonl row (matches log_metric's contract).
    step :
        Optional. See "Step semantics" in module docstring.

    Returns
    -------
    Decision
        Frozen dataclass. ``should_stop=False`` when no hook fired.
    """
    if not isinstance(metrics, dict):
        raise TypeError(
            f"runq.report: metrics must be a dict, got {type(metrics).__name__}"
        )

    ctx = get_ctx()
    if step is not None:
        ctx.current_step = step
        resolved_step: int | None = step
    else:
        resolved_step = ctx.current_step

    ts = int(time.time())

    # Emit one jsonl event per metric. Same format as log_metric so
    # daemon-side reap doesn't need to special-case report's output.
    # Active log_group prefix (if any) is applied per-key.
    for key, value in metrics.items():
        _append_event(
            {
                "type": "metric",
                "key": apply_prefix(str(key)),
                "value": float(value),
                "step": resolved_step,
                "ts": ts,
            }
        )

    # History entry — hooks see the *combined* metrics dict (not one
    # entry per key), which is what plateau / patience checks want.
    # NB: history keeps the user-facing key names (no prefix) so hook
    # code doesn't have to know about log_group.
    history_entry = {
        "metrics": dict(metrics),  # copy so hook mutation can't poison history
        "step": resolved_step,
        "ts": ts,
    }
    _history.append(history_entry)

    decision = _run_early_stop_hooks(metrics)
    # Step 8: loop() reads this on its next iteration.
    ctx._last_decision = decision
    return decision


# ---- hook orchestration -------------------------------------------

def _run_early_stop_hooks(current_metrics: dict) -> Decision:
    """Iterate registered hooks; produce a :class:`Decision`.

    Why short-circuit on the first truthy?
    - Predictable composition: hooks can be combined without thinking
      about merge semantics.
    - Side-effect safety: a logging hook that prints "stopping due to
      plateau" shouldn't ALSO be reached by a "max epochs" hook firing
      next.
    - Cheap: hook running cost stays O(rank of first stopper) not
      O(total hooks).

    Exception policy
    ----------------
    If a hook raises, propagate. User bugs in stopping logic should
    surface loudly, not get silently swallowed and "this loop never
    stops" mysteries later.

    Returns
    -------
    Decision
        ``should_stop=False`` when no hook fired truthy.
    """
    for hook in _hooks:
        result = hook(_history, current_metrics)
        if result is True:
            return Decision(should_stop=True, reason="user early_stop")
        if isinstance(result, str) and result.strip():
            return Decision(should_stop=True, reason=result)
    return Decision(should_stop=False, reason=None)


# ---- public introspection helpers ---------------------------------

def latest_report_metrics() -> dict:
    """Return the metrics dict from the most recent :func:`report` call.

    Returns ``{}`` if ``report`` has not been called yet. The dict is a
    snapshot — the caller is free to merge / mutate without poisoning
    runq's history. Used by ``ctx.wandb_like_metric`` and by anyone
    else who needs "what did I just report?" without holding the
    history list themselves.
    """
    if not _history:
        return {}
    return dict(_history[-1]["metrics"])


def history_snapshot() -> list[dict]:
    """Return a copy of the full report history.

    Each entry is ``{"metrics": dict, "step": int|None, "ts": int}``.
    Public for power-user inspection + tests; lazy users never need it.
    """
    return list(_history)


# ---- testing helpers ----------------------------------------------

def _reset_for_tests() -> None:
    """Reset hook registry + history. Tests only."""
    _hooks.clear()
    _history.clear()


# Back-compat alias for tests that imported the old private name.
_get_history_for_tests = history_snapshot
