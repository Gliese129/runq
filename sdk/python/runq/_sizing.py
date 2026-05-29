"""Estimate the upcoming-write size for ``safe_save``.

When the user doesn't pass ``size_hint`` explicitly, the SDK walks
their arguments to guess how many bytes the upcoming save will write.
The guess gates the disk-safety pre-check + drives the freeze
threshold sent to daemon.

Scope (step 4)
--------------
- ``torch.Tensor`` — ``numel × element_size``
- ``torch.nn.Module`` — sum of params + buffers
- ``dict`` — recurse into values
- ``list`` / ``tuple`` — recurse into items
- Everything else — ignored (contributes 0)

Why not include numpy / numbers / strings
-----------------------------------------
- numpy arrays could be added — but typical ML save patterns don't put
  raw numpy in checkpoints (they go through torch). Add if/when needed.
- Scalars (ints, floats) are basically free in pickle — ignoring them
  doesn't move the threshold meaningfully.
- Strings could be huge (tokenizers) but typical checkpoint dicts don't
  contain them at top level.

When the walker finds nothing
-----------------------------
Returns ``None`` to signal "couldn't estimate". Callers must then
require ``size_hint`` from the user. This is preferable to returning 0
(which would make the disk check a no-op).

Pickle overhead
---------------
The raw byte count from tensors is below the actual save size — pickle
adds headers, type names, version info. Empirically ~5-10%. We multiply
by 1.1 as a conservative overhead factor inside ``estimate_size``.
"""
from __future__ import annotations

from typing import Any

# Conservative multiplier for pickle / torch.save framing overhead.
# 1.1 = +10% on top of raw tensor bytes.
_PICKLE_OVERHEAD = 1.1


def estimate_size(*args: Any, **kwargs: Any) -> int | None:
    """Walk ``args`` + ``kwargs`` and sum bytes of recognized objects.

    Returns
    -------
    Optional[int]
        Estimated bytes (including ``_PICKLE_OVERHEAD`` multiplier) when
        at least one recognized object was found. ``None`` when the
        walk produced 0 bytes — caller treats this as "couldn't
        estimate, ask user for size_hint".

    Recognized object types: see module docstring.
    """
    total = 0
    seen = set() # avoid shared weight double count
    try:
        import torch
    except ImportError:
        torch = None # for test usage / non-torch validation / corner case
    def walk(x):
        nonlocal total
        # some architectures use shared weight for multihead blocks or ffn blocks
        if id(x) in seen:
            return
        seen.add(id(x))

        if torch is not None:
            if isinstance(x, torch.Tensor):
                # leaf node
                total += x.numel() * x.element_size()
                return
            if isinstance(x, torch.nn.Module):
                for t in x.state_dict().values():
                    walk(t)
                return

        if isinstance(x, dict):
            for v in x.values():
                walk(v)
            return
        if isinstance(x, (list, tuple, set)):
            for v in x:
                walk(v)
            return

        if hasattr(x, '__dict__'):
            for v in x.__dict__.values():
                walk(v)
            return

    for a in args:
        walk(a)
    for v in kwargs.values():
        walk(v)

    if total == 0:
        return None
    return int(total * _PICKLE_OVERHEAD)
