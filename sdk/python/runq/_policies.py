"""Built-in early-stop policies + YAML auto-registration.

Step 7 of the stage-2 SDK. The user's lazy path is a YAML block in the
job config (translated to ``params.json["early_stopping"]`` by the
daemon):

.. code-block:: yaml

    early_stopping:
      policy: patience       # or "threshold" / "convergence"
      metric: val_loss
      mode: min              # only for patience
      patience: 5
      min_steps: 10

When ``runq.context()`` initializes and finds such a block, it builds a
hook from the matching factory and registers it with
:mod:`runq._report`. The user writes zero Python for stopping logic.

Override path
-------------
If the user ALSO writes ``@runq.early_stop`` in code, the decorator
removes any YAML-registered hook and logs a WARNING. Reason: two
sources of truth for "when to stop" is a debugging hell. We commit to
"code wins, yaml is a default".

Why factories return functions instead of class instances
---------------------------------------------------------
Hooks are plain callables — same shape as user-decorated functions —
so the orchestration core in :mod:`runq._report` doesn't have to
special-case anything. Factories close over the policy params; the
returned function carries them via closure.
"""
from __future__ import annotations

import logging
import statistics
from typing import Any, Optional

from ._report import EarlyStopHook, _YAML_POLICY_MARK, _hooks


_LOG = logging.getLogger(__name__)


# ---- helpers ------------------------------------------------------

def _extract_series(history: list, metric: str) -> list[float]:
    """Pull the time series of ``metric`` from history.

    Skips entries that don't have the metric (e.g. epoch-only reports
    that didn't include this key). Order preserved.
    """
    return [
        e["metrics"][metric]
        for e in history
        if metric in e.get("metrics", {})
    ]


# ---- patience -----------------------------------------------------

def patience(
    *,
    metric: str,
    mode: str = "min",
    patience: int = 5,
    min_steps: int = 0,
) -> EarlyStopHook:
    """Hook factory: stop when ``metric`` hasn't improved for ``patience`` steps.

    Parameters
    ----------
    metric :
        Name of the metric to watch (e.g. ``"val_loss"``).
    mode :
        ``"min"`` if lower is better (loss), ``"max"`` if higher is
        better (accuracy).
    patience :
        Number of consecutive steps without improvement before stopping.
    min_steps :
        Don't stop before this many history entries exist. Lets early
        warmup variance ride out.

    Algorithm
    ---------
    Walk the metric time series (skipping reports that don't carry the
    metric) and track the best value seen so far. When the gap between
    the most recent index and the last-improvement index reaches
    ``patience``, fire. Entries below ``min_steps`` are held off.

    Edge cases:
    - All values equal → no improvement seen; idle counter grows from
      index 0 onwards.
    - First value is automatically the best until something better
      shows up.
    """
    if mode not in ("min", "max"):
        raise ValueError(f"runq.patience: mode must be 'min' or 'max', got {mode!r}")
    if patience < 1:
        raise ValueError(f"runq.patience: patience must be >= 1, got {patience}")
    if min_steps < 0:
        raise ValueError(f"runq.patience: min_steps must be >= 0, got {min_steps}")

    def hook(history: list, current: dict):
        values = _extract_series(history, metric)
        if len(values) < max(1, min_steps):
            return False
        better = (lambda new, best: new < best) if mode == "min" else (lambda new, best: new > best)
        best_idx, best_val = 0, values[0]
        for i, v in enumerate(values[1:], start=1):
            if better(v, best_val):
                best_val, best_idx = v, i
        idle = (len(values) - 1) - best_idx
        return f"patience exhausted: {metric} no improvement in {patience} steps" if idle >= patience else False

    hook.__name__ = f"patience({metric},{mode},{patience})"
    return hook


# ---- threshold ----------------------------------------------------

def threshold(
    *,
    metric: str,
    direction: str,
    bound: float,
) -> EarlyStopHook:
    """Hook factory: stop when ``metric`` crosses ``bound`` in ``direction``.

    Parameters
    ----------
    metric : name of metric to watch.
    direction : ``"above"`` or ``"below"``.
    bound : numeric threshold.

    Returns ``True``-ish reason when:
    - direction='above' and current[metric] >= bound, OR
    - direction='below' and current[metric] <= bound.

    Reads ``current`` only — no history needed. If the metric isn't in
    the current report, nothing fires (other reports may have it).
    """
    if direction not in ("above", "below"):
        raise ValueError(
            f"runq.threshold: direction must be 'above' or 'below', "
            f"got {direction!r}"
        )

    def hook(history: list, current: dict):
        if metric not in current:
            return False
        v = current[metric]
        if direction == "above" and v >= bound:
            return f"{metric}={v} crossed above {bound}"
        if direction == "below" and v <= bound:
            return f"{metric}={v} crossed below {bound}"
        return False

    hook.__name__ = f"threshold({metric},{direction},{bound})"
    return hook


# ---- convergence --------------------------------------------------

def convergence(
    *,
    metric: str,
    window: int = 5,
    epsilon: float = 1e-4,
) -> EarlyStopHook:
    """Hook factory: stop when ``metric`` stddev over ``window`` last entries < ``epsilon``.

    Parameters
    ----------
    metric : name to watch.
    window : how many recent values to compute stddev over.
    epsilon : stop when stddev drops below this.

    Algorithm
    ---------
    Sample stddev (``statistics.stdev``, n-1) over the last ``window``
    values of the metric. Below ``epsilon`` → fire. ``window`` is
    validated ≥ 2 at construction so the n-1 denominator never blows
    up.
    """
    if window < 2:
        raise ValueError(f"runq.convergence: window must be >= 2, got {window}")
    if epsilon <= 0:
        raise ValueError(f"runq.convergence: epsilon must be > 0, got {epsilon}")

    def hook(history: list, current: dict):
        values = _extract_series(history, metric)
        if len(values) < window:
            return False
        if statistics.stdev(values[-window:]) < epsilon:
            return f"converged: stddev<{epsilon} over last {window} steps"
        return False

    hook.__name__ = f"convergence({metric},win={window},eps={epsilon})"
    return hook


# ---- YAML / params auto-registration ------------------------------

# Maps policy name strings to factory functions. Add a new built-in
# here when its factory ships.
_POLICY_FACTORIES = {
    "patience": patience,
    "threshold": threshold,
    "convergence": convergence,
}


def maybe_register_from_params(ctx: Any) -> Optional[str]:
    """Read ``ctx.params['early_stopping']`` and register a built-in hook.

    Returns the policy name actually registered, or ``None`` if no
    ``early_stopping`` block was present.

    Schema expected (matches the YAML / job-config doc)::

        ctx.params = {
            ...,
            "early_stopping": {
                "policy": "patience",   # optional, defaults to "patience"
                "metric": "val_loss",
                "mode": "min",
                "patience": 5,
                ...
            }
        }

    The block's other keys are passed to the chosen factory as
    keyword args. Unknown keys → ``TypeError`` from the factory, which
    we re-raise as a more informative ``ValueError`` so the user sees
    "your job config has a typo" rather than "patience() got an
    unexpected keyword argument".

    Marker
    ------
    The returned hook is tagged with :data:`_YAML_POLICY_MARK` so that
    a later ``@runq.early_stop`` can identify and remove it.
    """
    if not isinstance(ctx.params, dict):
        return None
    block = ctx.params.get("early_stopping")
    if not block:
        return None
    if not isinstance(block, dict):
        raise ValueError(
            f"runq: early_stopping must be a dict, got {type(block).__name__}"
        )

    # Copy so we can pop "policy" without mutating the user's params.
    cfg = dict(block)
    policy_name = cfg.pop("policy", "patience")
    factory = _POLICY_FACTORIES.get(policy_name)
    if factory is None:
        raise ValueError(
            f"runq: unknown early_stopping policy {policy_name!r}; "
            f"known: {sorted(_POLICY_FACTORIES)}"
        )

    try:
        hook = factory(**cfg)
    except TypeError as e:
        raise ValueError(
            f"runq: invalid early_stopping config for policy "
            f"{policy_name!r}: {e}"
        ) from e

    # Marker the report.early_stop decorator checks to know "this came
    # from YAML, not user code".
    setattr(hook, _YAML_POLICY_MARK, True)
    setattr(hook, "_runq_yaml_policy_name", policy_name)

    if hook not in _hooks:
        _hooks.append(hook)
    return policy_name
