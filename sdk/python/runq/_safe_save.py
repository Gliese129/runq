"""Disk-safe checkpoint save with TOCTOU defense + daemon freeze flow.

This is step 3 of stage 2: the function-form ``safe_save(path, obj, ...)``
with the full atomic-rename + ENOSPC-catch + freeze algorithm. Steps 4
& 5 layer on the decorator + size walker + manifest cleanup.

Why this is non-trivial
-----------------------
The naive approach — "check disk, then call torch.save" — has a race:
two sibling tasks both pass the pre-check, both start saving, one of
them hits ENOSPC midway through and leaves a partial file at
``ckpt.pt``. Next time the user loads that file, torch chokes on a
truncated pickle.

The TOCTOU-safe pattern uses three guards in combination:

1. **Pre-flight check** — fast path that avoids freeze when disk is
   clearly fine. Not authoritative (race may invalidate it).
2. **Tmp file + atomic rename** — write to ``ckpt.pt.runq-tmp-NNN``
   in the same directory, fsync, then rename. ``ckpt.pt`` either
   contains the previous version or the new one, never partial.
3. **ENOSPC catch + retry** — if the save raises ``OSError(ENOSPC)``
   mid-write, clean up the tmp file, enter the freeze flow, retry the
   whole save from scratch.

Freeze flow
-----------
- daemon mode: POST ``/api/internal/freeze-self``. The daemon SIGSTOPs
  this task's pgroup; the HTTP recv blocks. When the user runs
  ``runq thaw`` (or auto-thaw fires), SIGCONT resumes, recv returns,
  and we loop back to re-check disk + retry the save.
- ``no_daemon`` mode: no daemon to call → raise
  :class:`RunqDiskFullError`. HPC user's task dies; that's the
  contract documented in the mode matrix.

Manifest + cleanup
------------------
NOT in step 3. step 5 adds ``keep_last_n`` / ``keep_best`` cleanup with
a manifest-scoped delete (only files runq created). For now, every
successful save just appends a ``checkpoint`` event to metrics.jsonl
and leaves the file alone.
"""
from __future__ import annotations

import errno
import functools
import inspect
import os
import shutil
import time
from collections.abc import Callable
from pathlib import Path
from typing import Any

from . import _manifest
from ._context import Context, get_ctx
from ._events import _append_event
from ._exceptions import RunqDiskFullError
from ._sizing import estimate_size
from ._transport import post_json

# A save function is anything that writes `obj` to `path`. Default in
# production is torch.save; tests pass small lambdas to avoid the
# torch dependency.
SaveFn = Callable[[str, Any], None]

# Sentinel for distinguishing "user passed obj=None on purpose" from
# "user called safe_save(some_fn)" (decorator form, no obj arg).
_NO_OBJ = object()

# Decorator strips these from forwarded kwargs unless the user's
# function explicitly declares them as parameters.
_MANAGED_KWARGS = ("step", "is_best", "size_hint")


def _default_save(path: str, obj: Any) -> None:
    """Fallback when no save_fn is provided. Uses torch.save.

    Lazy import so users who pass save_fn explicitly never need torch
    on the import path.
    """
    try:
        import torch  # type: ignore
    except ImportError as e:
        raise ImportError(
            "runq.safe_save: torch not available; either install torch "
            "or pass save_fn=your_save_function"
        ) from e
    torch.save(obj, path)


# ---- helpers ----

def _resolve_path(path: str | Path) -> str:
    """Resolve `path` to an absolute filesystem path.

    Rules (per stage2_sdk_design.md §F2 "Path resolution"):
    - Absolute path → return as-is (after str conversion).
    - Relative path → join with ``ctx.checkpoint_dir`` if set, else cwd.

    Always returns a ``str`` (downstream ``os.rename`` / ``torch.save``
    accept both ``Path`` and ``str`` but ``str`` keeps the type contract
    simple).
    """
    path = str(path)
    if os.path.isabs(path):
        return path
    ctx = get_ctx()
    base_dir = ctx.checkpoint_dir if ctx.checkpoint_dir is not None else Path.cwd()
    return os.path.join(base_dir, path)


def _resolve_mountpoint(path: str) -> str:
    """Find the mountpoint that ``path`` lives on.

    Used as the ``mount`` field in the freeze-self request. The daemon keys its
    per-mount freeze state on this value, so SDK callers from the same disk MUST
    resolve to the same string. Walking up via ``os.path.ismount`` gives that
    consistency without parsing /proc/mounts.

    The path itself may not exist yet (e.g., if we are about to create it). To
    handle this safely, the function first walks up to the nearest existing
    parent directory, resolves any symbolic links at that stable point, and then
    continues walking up to locate the mountpoint.

    This specific execution order addresses a critical edge case involving broken
    symbolic links where a naive implementation (resolving realpath before walking
    up) would diverge.

    Edge Case Example:
        Given a broken symlink:
            /mnt/a/broken -> /mnt/b/not_exist
        And a query path:
            /mnt/a/broken/file.pt

        - Naive approach (realpath first):
          Fails to resolve the non-existent path, defaults to literal walk-up,
          stops at the existing link parent, and incorrectly returns the mount
          for '/mnt/a'.
        - This approach (walk up first):
          Strips the non-existent 'file.pt', stops at the existing symlink file
          '/mnt/a/broken', successfully resolves it to '/mnt/b/not_exist', and
          then correctly returns the mount for '/mnt/b'.
    """
    p = path
    while p and not os.path.exists(p):
        parent = os.path.dirname(p)
        if parent == p:
            break
        p = parent
    p = os.path.realpath(p)
    while not os.path.ismount(p):
        parent = os.path.dirname(p)
        if parent == p:
            break
        p = parent
    return p


def _compute_threshold(size_bytes: int, ctx: Context) -> int:
    """Compute the free-bytes threshold required before a save.

    Formula: ``size_bytes × safety_factor_percent / 100 + safety_extra_gb × 1 GiB``
    """
    return (
        int(size_bytes * ctx.safety_factor_percent / 100)
        + ctx.safety_extra_gb * 1024 * 1024 * 1024
    )


def _handle_disk_short(
    ctx: Context,
    *,
    free_bytes: int,
    needed_bytes: int,
    mount: str,
) -> None:
    """Disk-shortage handler. Mode-dependent.

    daemon mode:
        POST /api/internal/freeze-self with the diagnostic payload.
        The daemon SIGSTOPs us, so this call blocks until SIGCONT
        (i.e. until user runs ``runq thaw`` or auto-thaw fires).
        Returns normally after thaw — caller should re-check disk
        and retry the save.

    no_daemon mode:
        No daemon to coordinate with. Raise RunqDiskFullError with
        diagnostic fields so the caller can decide whether to abort
        the task or fall back.

    Raises
    ------
    RunqDiskFullError
        no_daemon mode.
    TransportError
        daemon mode but daemon unreachable or returned non-2xx.
    """
    assert ctx.mode in ["daemon", "no_daemon", "manual"]
    if ctx.mode == "daemon":
        assert ctx.socket_path
        post_json(ctx.socket_path, '/api/internal/freeze-self', {
            'task_id': ctx.task_id,
            'free_bytes': free_bytes,
            'needed_est': needed_bytes,
            'mount': mount
        })
    else:
        raise RunqDiskFullError(
            mount=mount, free_bytes=free_bytes, needed_bytes=needed_bytes
        )



# ---- decorator form (step 4 / step 5) ----

def _make_decorator(
    user_fn: Callable[..., None],
    *,
    keep_last_n: int | None = None,
    keep_best: bool = False,
) -> Callable[..., None]:
    """Wrap ``user_fn`` so its calls go through the TOCTOU + freeze flow.

    Returned wrapper has signature ``wrapper(path, *user_args, **user_kwargs)``.
    At call time:

    1. The runq-managed kwargs (``step``, ``is_best``, ``size_hint``) are
       pulled out for the SDK's own use (jsonl event, threshold).
    2. If the user's function declared the same kwarg name in its own
       signature, the kwarg ALSO gets forwarded so the user sees the
       same value. Otherwise the kwarg is stripped before forwarding.
       This prevents "your function got an unexpected keyword 'step'"
       errors while still letting users see runq-managed values when
       they want.
    3. If ``size_hint`` is missing, the SDK walks ``user_args`` +
       remaining ``user_kwargs`` via ``estimate_size``. Missing AND
       walker found nothing → TypeError telling user to install torch
       or pass size_hint=N.
    4. Delegates to the function form of ``safe_save`` with a save_fn
       that calls user_fn with the forwarded args. ``keep_last_n`` /
       ``keep_best`` baked at decorator-construction time pass through
       to the function form, which handles manifest append + cleanup.
    """
    sig_params = set(inspect.signature(user_fn).parameters)

    # preserve __name__ and __doc__
    @functools.wraps(user_fn)
    def wrapper(path, *user_args, **user_kwargs):
        runq_step = user_kwargs.get("step")
        runq_is_best = user_kwargs.get("is_best", False)
        runq_size_hint = user_kwargs.get("size_hint")
        # strip runq managed kwargs, unless user set them manually
        forwarded = dict(user_kwargs)
        for name in _MANAGED_KWARGS:
            if name not in sig_params and name in forwarded:
                del forwarded[name]

        if runq_size_hint is None:
            estimated = estimate_size(*user_args, **user_kwargs)
            if estimated is None:
                raise TypeError(
                    "runq.safe_save: couldn't estimate save size; "
                    "pass size_hint or install torch"
                )
            runq_size_hint = estimated

        def save_fn(tmp_path: str, _unused: Any) -> None:
            user_fn(tmp_path, *user_args, **forwarded)

        safe_save(
            path, None,
            save_fn=save_fn,
            step=runq_step,
            is_best=runq_is_best,
            size_hint=runq_size_hint,
            keep_last_n=keep_last_n,
            keep_best=keep_best,
            _saved_by=user_fn.__name__,
        )
    return wrapper

# ---- main API ----

def safe_save(
    path_or_fn=_NO_OBJ,
    obj: Any = _NO_OBJ,
    *,
    save_fn: SaveFn | None = None,
    step: int | None = None,
    is_best: bool = False,
    size_hint: int | None = None,
    keep_last_n: int | None = None,
    keep_best: bool = False,
    _saved_by: str | None = None,
) -> None:
    """Save ``obj`` to ``path`` with disk-safety guards.

    Three forms, dispatched by the first argument:

    1. **Function form** — ``safe_save(path, obj, ...)``: SDK saves
       ``obj`` to ``path`` using ``save_fn`` (default torch.save).
    2. **Bare decorator** — ``@runq.safe_save`` on a user function: the
       user's function does the actual write; SDK wraps it with the
       TOCTOU + freeze logic.
    3. **Parameterized decorator** — ``@runq.safe_save(keep_last_n=3,
       keep_best=True)``: like (2), but with retention policy baked in
       at decorator-construction time.

    Function form parameters
    ------------------------
    path :
        Where to save. Relative paths resolve to ``ctx.checkpoint_dir``;
        absolute paths are used as-is.
    obj :
        The thing to save. Passed to ``save_fn`` (default torch.save).
    save_fn :
        Custom save function ``(path: str, obj: Any) -> None``. Used
        primarily by tests; production users rely on the torch default.
    step :
        Step number recorded in the checkpoint event + manifest. F5.5
        ownership rules: explicit ``step=N`` writes back to
        ``ctx.current_step``; ``step=None`` falls back to
        ``ctx.current_step`` (which ``runq.loop()`` set on the current
        iteration). ``step=0`` is a valid explicit value; only
        ``None`` triggers the ctx fallback.
    is_best :
        Mark this checkpoint as the current best in the metrics event.
        Triggers manifest's single-best invariant: prior entries lose
        the flag.
    size_hint :
        Bytes the save is expected to write. When None, the SDK walks
        ``obj`` via :func:`runq._sizing.estimate_size`. If the walker
        can't recognize anything, ``TypeError`` asks the user to either
        pass an explicit ``size_hint`` or install torch.
    keep_last_n :
        Optional retention policy. After this save succeeds, keep at
        most the N most-recent runq-managed checkpoints in
        ``ctx.checkpoint_dir`` (by ``(step, ts)`` desc). Manifest-
        scoped — only deletes files runq itself created. User-placed
        sibling files are NEVER touched.

        - ``None`` (default) → no quantity-based cleanup.
        - ``0`` → only valid alongside ``keep_best=True`` ("best only").
        - ``N > 0`` → rolling window of N checkpoints.
    keep_best :
        Additionally preserve the most-recent ``is_best=True``
        checkpoint, even if ``keep_last_n`` would evict it.

        REQUIRES an explicit ``keep_last_n`` (int, ≥0). The combination
        ``keep_best=True, keep_last_n=None`` is rejected with
        ``ValueError`` because it admits two contradictory readings
        ("only the best" vs "everything + best is a no-op"). Spell it
        out:

        - ``keep_last_n=0,  keep_best=True`` → best only.
        - ``keep_last_n=10, keep_best=True`` → last 10 + ensure best.

    Decorator forms
    ---------------
    ``@runq.safe_save`` — no parens, no policy. Function below is
    wrapped with the TOCTOU/freeze flow only.

    ``@runq.safe_save(keep_last_n=3, keep_best=True)`` — parens, policy
    baked in; every call to the wrapped function appends a manifest
    entry and runs cleanup.

    Raises
    ------
    RunqDiskFullError
        no_daemon mode with insufficient disk and no freeze possible.
    TransportError
        daemon mode but daemon unreachable.
    TypeError
        size_hint=None AND walker couldn't estimate (no torch, no
        recognized types in args), or invalid dispatch arguments.
    ValueError
        Invalid retention policy combination — e.g. ``keep_best=True``
        without an explicit ``keep_last_n``, or ``keep_last_n < 0``.
    OSError
        Filesystem error other than ENOSPC (permission denied, etc.).
    """
    # Policy validation up-front, before any dispatch branch. Fail-fast
    # so users see the error at @runq.safe_save(...) construction or at
    # the first call site, not buried two saves deep inside cleanup.
    _manifest.validate_policy(keep_last_n, keep_best)

    # ---- dispatch ----
    # Case (3): `safe_save(keep_last_n=..., keep_best=...)` with NO
    # positional arg at all → parameterized decorator factory. Return a
    # decorator-of-fn that calls back into _make_decorator with the
    # policy baked in.
    if path_or_fn is _NO_OBJ:
        if obj is not _NO_OBJ:
            raise TypeError(
                "runq.safe_save: missing path / function argument"
            )

        def _factory(user_fn):
            if not callable(user_fn) or isinstance(user_fn, (str, bytes, os.PathLike)):
                raise TypeError(
                    "runq.safe_save(...) must be used as a decorator on a function"
                )
            return _make_decorator(
                user_fn, keep_last_n=keep_last_n, keep_best=keep_best
            )
        return _factory  # type: ignore[return-value]

    # Case (2): bare decorator — `@runq.safe_save` passes the function as
    # path_or_fn. PathLike check defends against the unlikely path-that-
    # is-callable.
    if callable(path_or_fn) and not isinstance(path_or_fn, (str, bytes, os.PathLike)):
        if obj is not _NO_OBJ:
            raise TypeError(
                "runq.safe_save used as @decorator does not accept positional "
                "args here — pass them at the call site of the decorated function"
            )
        return _make_decorator(
            path_or_fn, keep_last_n=keep_last_n, keep_best=keep_best
        )  # type: ignore[return-value]

    # ---- function form ----
    if obj is _NO_OBJ:
        raise TypeError("runq.safe_save(path, obj) requires `obj`")
    path = path_or_fn

    # Auto-estimate size_hint if not provided.
    if size_hint is None:
        estimated = estimate_size(obj)
        if estimated is None:
            raise TypeError(
                "runq.safe_save: couldn't estimate save size from obj; "
                "pass size_hint=N or install torch"
            )
        size_hint = estimated

    ctx = get_ctx()
    # F5.5 step ownership: explicit step wins and writes back; missing
    # step falls back to whatever loop()/report() last set on ctx.
    # Mirrors the report() / log_metric() contract so all three step-
    # aware APIs stay in sync.
    if step is not None:
        ctx.current_step = step
    else:
        step = ctx.current_step
    final_path = _resolve_path(path)
    dir_for_usage = os.path.dirname(final_path) or '/'
    # Auto-create the parent directory. Without this, users have to
    # mkdir before every nested save (e.g. "epoch-5/model.pt"). Safe
    # to call repeatedly thanks to exist_ok=True; cost is one stat.
    os.makedirs(dir_for_usage, exist_ok=True)
    mount = _resolve_mountpoint(str(path))
    effective_save_fn = save_fn or _default_save
    tmp_path = f"{final_path}.runq-tmp-{os.getpid()}-{time.time_ns()}"
    threshold = _compute_threshold(size_hint, ctx)

    while True:
        # pre-check
        free = shutil.disk_usage(dir_for_usage).free
        if free < threshold:
            _handle_disk_short(ctx, free_bytes=free, needed_bytes=threshold, mount=mount)
            continue
        # save with ENOSPC defense
        # atomic write with tmp file
        try:
            effective_save_fn(tmp_path, obj)
            fd = os.open(tmp_path, os.O_RDONLY)
            try:
                os.fsync(fd)
            finally:
                os.close(fd)
            os.replace(tmp_path, final_path)
            dir_fd = os.open(os.path.dirname(final_path), os.O_RDONLY)
            try:
                os.fsync(dir_fd)
            except OSError:
                # Rare case, some old machines don't support fsync a directory
                pass
            finally:
                os.close(dir_fd)
            break
        except OSError as e:
            if e.errno in (errno.ENOSPC, errno.EDQUOT):
                # Mid-save disk exhaustion

                try:
                    os.unlink(tmp_path)
                except FileNotFoundError:
                    pass
                free = shutil.disk_usage(dir_for_usage).free
                # daemon mode: http connection will block it for several seconds
                # other  mode: raise error instantly
                _handle_disk_short(ctx, free_bytes=free, needed_bytes=threshold, mount=mount)
                continue
            # Other error: no retry
            try:
                os.unlink(tmp_path)
            except FileNotFoundError:
                pass
            raise
    # Best-effort actual size for the metric event.
    try:
        actual_bytes = os.path.getsize(final_path)
    except OSError:
        # fallback
        actual_bytes = size_hint
    now_ts = int(time.time())
    _append_event({
        "type": "checkpoint",
        "path": final_path,
        "size_bytes": actual_bytes,
        "step": step,  # None is fine; daemon-side reap tolerates it
        "is_best": is_best,
        "ts": now_ts,
    })

    # ---- manifest append + optional cleanup (step 5) ----
    # Only track saves landing under ctx.checkpoint_dir. If user wrote
    # to an absolute path elsewhere, we do nothing — that's not a
    # runq-managed location and cleanup must not touch it.
    if ctx.checkpoint_dir is not None:
        rel_key = _manifest.to_manifest_key(ctx.checkpoint_dir, final_path)
        if rel_key is not None:
            entry = {
                "path": rel_key,
                "step": step,
                "is_best": bool(is_best),
                "size_bytes": int(actual_bytes) if actual_bytes is not None else None,
                "ts": now_ts,
                "saved_by": _saved_by,
            }
            try:
                _manifest.append_entry(ctx.checkpoint_dir, entry)
            except NotImplementedError:
                # Pre-step-5 build: manifest core not implemented yet.
                # Tests that exercise the manifest path explicitly
                # implement append_entry; everything else silently
                # skips the bookkeeping rather than crashing the save.
                pass
            else:
                if (keep_last_n is not None and keep_last_n > 0) or keep_best:
                    try:
                        _manifest.cleanup(
                            ctx.checkpoint_dir,
                            keep_last_n=keep_last_n,
                            keep_best=keep_best,
                        )
                    except NotImplementedError:
                        pass
    return None
