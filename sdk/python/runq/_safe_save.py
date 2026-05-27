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
import os
import shutil
import time
from pathlib import Path
from typing import Any, Callable, Optional, Union

from ._context import Context, get_ctx
from ._events import _append_event
from ._exceptions import RunqDiskFullError
from ._transport import TransportError, post_json


# A save function is anything that writes `obj` to `path`. Default in
# production is torch.save; tests pass small lambdas to avoid the
# torch dependency.
SaveFn = Callable[[str, Any], None]


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


# ---- helpers (TODO: implement bodies) ----

def _resolve_path(path: Union[str, Path]) -> str:
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
    return int(size_bytes * ctx.safety_factor_percent / 100) + ctx.safety_extra_gb * 1024 * 1024 * 1024


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



# ---- main API ----

def safe_save(
    path: Union[str, Path],
    obj: Any,
    *,
    save_fn: Optional[SaveFn] = None,
    step: Optional[int] = None,
    is_best: bool = False,
    size_hint: int,
) -> None:
    """Save ``obj`` to ``path`` with disk-safety guards.

    Step 3 of stage 2: function form only. Step 4 adds the decorator
    form + size auto-estimation; step 5 adds manifest cleanup. In step
    3 the caller must pass ``size_hint`` explicitly.

    Parameters
    ----------
    path :
        Where to save. Relative paths resolve to ``ctx.checkpoint_dir``;
        absolute paths are used as-is.
    obj :
        The thing to save. Passed to ``save_fn`` (default
        ``torch.save``).
    save_fn :
        Custom save function ``(path: str, obj: Any) -> None``. Used
        primarily by tests; production users rely on the torch default.
    step :
        Step number recorded in the checkpoint event. ``step=0`` is a
        valid value; only ``None`` means "no step".
    is_best :
        Mark this checkpoint as the current best in the metrics event.
        Step 5 will also persist this in the manifest.
    size_hint :
        Required. Bytes the save is expected to write. Used to compute
        the disk-safety threshold. Step 4 will auto-estimate from
        ``obj`` (torch.Tensor / nn.Module walker).

    Raises
    ------
    RunqDiskFullError
        no_daemon mode with insufficient disk and no freeze possible.
    TransportError
        daemon mode but daemon unreachable.
    OSError
        Filesystem error other than ENOSPC (permission denied, etc.).
    """
    ctx = get_ctx()
    final_path = _resolve_path(path)
    dir_for_usage = os.path.dirname(final_path) or '/'
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
    _append_event({
        "type": "checkpoint",
        "path": final_path,
        "size_bytes": actual_bytes,
        "step": step,  # None is fine; daemon-side reap tolerates it
        "is_best": is_best,
        "ts": int(time.time()),
    })
