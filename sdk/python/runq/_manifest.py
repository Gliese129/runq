"""Manifest of runq-created checkpoint files — backs ``keep_last_n`` / ``keep_best``.

Why a manifest at all?
----------------------
``keep_last_n`` could in principle ``glob("*.pt")`` the checkpoint
directory and delete the oldest matches. Don't do that. Lab users put
all sorts of stuff alongside their checkpoints:

- adhoc snapshots they want to keep across cleanups
- config dumps, plot PDFs, profiling traces
- pre-runq checkpoints from a previous run

A glob-based cleaner would happily nuke any of those.

The manifest is the SDK's own ledger of what IT created. Cleanup only
deletes files that appear in this ledger. User-placed siblings are
invisible to the policy.

File layout
-----------
``<ctx.checkpoint_dir>/.runq_manifest.json``

Schema (version 1)::

    {
      "version": 1,
      "entries": [
        {
          "path": "ckpt-1.pt",     # relative to checkpoint_dir
          "step": 1,               # may be null
          "is_best": false,
          "size_bytes": 1024,
          "ts": 1700000000
        },
        ...
      ]
    }

Failure modes
-------------
Corrupt / unreadable manifest → start fresh, log a warning. We never
crash the user's training job because of our own bookkeeping file.

``is_best`` invariant
---------------------
At most one entry has ``is_best=True`` at any given moment. When a new
entry with ``is_best=True`` is appended, the flag is cleared on all
prior entries.

Atomic writes
-------------
Manifest writes follow the same tmp+fsync+rename pattern as
``safe_save`` itself — corrupt manifest after a crash mid-write is
the only way the ledger could go out of sync with disk, so we close
that hole.
"""
from __future__ import annotations

import json
import logging
import os
import shutil
import time
from pathlib import Path
from typing import Any, Optional, Union

_LOG = logging.getLogger(__name__)

MANIFEST_FILENAME = ".runq_manifest.json"
MANIFEST_VERSION = 1


# ---- policy validation --------------------------------------------

def validate_policy(keep_last_n: Optional[int], keep_best: bool) -> None:
    """Reject ambiguous policy combinations early.

    Why ``keep_best=True`` REQUIRES an explicit ``keep_last_n``
    ~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~
    ``keep_best=True, keep_last_n=None`` admits two reasonable readings:

    1. "Keep only the best, drop everything else."
    2. "Keep everything (no quantity cap) plus mark the best — i.e.
       effectively a no-op since 'everything' already includes the best."

    Reading (1) silently deletes user data; reading (2) silently does
    nothing useful. Either way the user gets a surprise. We refuse the
    combination at policy-set time and force them to write what they
    mean:

    - ``keep_last_n=0,  keep_best=True``  → "Best only, drop the rest."
    - ``keep_last_n=10, keep_best=True``  → "Last 10 + ensure best."

    ``keep_last_n=0`` IS valid
    ~~~~~~~~~~~~~~~~~~~~~~~~~~
    Manifest-scoped delete means even ``N=0`` is safe — we only ever
    touch files we created. The user is saying "rotate this slot,
    don't keep history". Honor it.
    """
    if keep_best and keep_last_n is None:
        raise ValueError(
            "runq.safe_save: keep_best=True requires an explicit "
            "keep_last_n (use keep_last_n=0 to keep only the best). "
            "keep_last_n=None + keep_best=True is ambiguous and not "
            "supported."
        )
    if keep_last_n is not None and keep_last_n < 0:
        raise ValueError(
            f"runq.safe_save: keep_last_n must be >= 0, got {keep_last_n}"
        )


# ---- mechanical: paths + atomic IO ----------------------------------

def manifest_path(checkpoint_dir: Union[str, Path]) -> Path:
    return Path(checkpoint_dir) / MANIFEST_FILENAME


def _empty_manifest() -> dict:
    return {"version": MANIFEST_VERSION, "entries": []}


def load_manifest(checkpoint_dir: Union[str, Path]) -> dict:
    """Read manifest from disk.

    Returns a fresh empty manifest if:
    - the file does not exist (first save), OR
    - the file is unreadable / not valid JSON, OR
    - the schema version doesn't match what this SDK knows.

    We deliberately do NOT raise on a corrupt manifest. Reason: this is
    a bookkeeping file. Crashing the user's training job because we
    can't parse our own ledger would be hostile. We log and move on;
    pre-existing files become invisible to cleanup, which is the safe
    failure mode (we don't delete things we don't recognize).
    """
    mp = manifest_path(checkpoint_dir)
    if not mp.exists():
        return _empty_manifest()
    try:
        raw = mp.read_text()
        data = json.loads(raw)
    except (OSError, json.JSONDecodeError) as e:
        _LOG.warning("runq: manifest at %s unreadable (%s); starting fresh", mp, e)
        return _empty_manifest()
    if not isinstance(data, dict) or data.get("version") != MANIFEST_VERSION:
        _LOG.warning(
            "runq: manifest at %s schema/version mismatch (got %r); starting fresh",
            mp, data.get("version") if isinstance(data, dict) else type(data).__name__,
        )
        return _empty_manifest()
    if not isinstance(data.get("entries"), list):
        return _empty_manifest()
    return data


def save_manifest(checkpoint_dir: Union[str, Path], manifest: dict) -> None:
    """Atomic write: tmp + fsync + rename.

    Same pattern as ``safe_save``'s file body, scoped to the manifest
    file. A crash mid-write leaves either the old manifest or the new
    one — never a half-written JSON file that would force the
    next-start codepath to re-init from scratch.
    """
    mp = manifest_path(checkpoint_dir)
    mp.parent.mkdir(parents=True, exist_ok=True)
    tmp = mp.with_name(mp.name + f".tmp-{os.getpid()}-{time.time_ns()}")
    payload = json.dumps(manifest, indent=2, ensure_ascii=False)
    tmp.write_text(payload, encoding="utf-8")
    fd = os.open(tmp, os.O_RDONLY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)
    os.replace(tmp, mp)
    # Best-effort directory fsync so the rename is durable on power loss.
    try:
        dir_fd = os.open(str(mp.parent), os.O_RDONLY)
        try:
            os.fsync(dir_fd)
        except OSError:
            pass
        finally:
            os.close(dir_fd)
    except OSError:
        pass


# ---- helpers for path bookkeeping ----------------------------------

def to_manifest_key(checkpoint_dir: Union[str, Path], final_path: Union[str, Path]) -> Optional[str]:
    """Compute the manifest's ``entries[i].path`` value for ``final_path``.

    Returns the path relative to ``checkpoint_dir`` if the file lives
    under that directory. Returns ``None`` if the user saved to a path
    outside ``checkpoint_dir`` — in that case the caller should skip
    manifest entry creation (we don't manage that file).

    Why relative paths in the manifest?
    - Portable: moving the checkpoint dir doesn't invalidate the ledger.
    - Scope-clear: every entry's path is unambiguously a child of
      ``checkpoint_dir``, so cleanup can never traverse out via ``..``.
    """
    ckpt = Path(checkpoint_dir).resolve()
    fp = Path(final_path).resolve()
    try:
        rel = fp.relative_to(ckpt)
    except ValueError:
        return None
    return str(rel)


# ---- core operations (USER WRITES BODIES) --------------------------

def append_entry(
    checkpoint_dir: Union[str, Path],
    entry: dict,
) -> dict:
    """Append ``entry`` to the manifest. Persists and returns the updated manifest.

    Invariant — single best
    ~~~~~~~~~~~~~~~~~~~~~~~
    If ``entry["is_best"]`` is True, the flag MUST be cleared on every
    existing entry BEFORE appending. At most one entry has
    ``is_best=True`` at any time. (The new entry is the "current
    best"; older bests demote to plain entries.)

    Algorithm (write this body)::

        m = load_manifest(checkpoint_dir)
        if entry.get("is_best"):
            for e in m["entries"]:
                e["is_best"] = False
        m["entries"].append(entry)
        save_manifest(checkpoint_dir, m)
        return m

    Returns
    -------
    dict
        The updated manifest. Caller can pass it straight into
        :func:`cleanup` to avoid a second disk read.
    """
    m = load_manifest(checkpoint_dir)
    if entry.get("is_best"):
        for e in m["entries"]:
            e["is_best"] = False
    m["entries"].append(entry)
    save_manifest(checkpoint_dir, m)
    return m


def cleanup(
    checkpoint_dir: Union[str, Path],
    *,
    keep_last_n: Optional[int] = None,
    keep_best: bool = False,
) -> list[str]:
    """Apply retention policy. Delete checkpoints NOT in the keep set.

    **Manifest-scoped deletion**: this function only deletes files that
    appear in ``manifest["entries"]``. User-placed sibling files in the
    checkpoint directory (configs, plot PDFs, ad-hoc snapshots) are
    NEVER touched. This is the safety invariant the whole module exists
    to maintain — do not weaken it.

    Parameters
    ----------
    keep_last_n :
        Keep at most ``N`` most-recent checkpoints by ``(step, ts)`` desc.

        - ``None`` → no quantity cap; every entry survives (the function
          essentially just rewrites the manifest as-is).
        - ``0`` → keep zero by quantity. Only valid alongside
          ``keep_best=True`` (otherwise it's "delete everything",
          which the user can do faster by rm-rf'ing the dir).
        - ``N > 0`` → keep the N highest ``(step, ts)`` entries.
    keep_best :
        Additionally preserve the **most-recent** entry with
        ``is_best=True``, even if it would be evicted by
        ``keep_last_n``.

        REQUIRES an explicit ``keep_last_n`` value (int, ≥0). The
        combination ``keep_best=True, keep_last_n=None`` is ambiguous
        and rejected with ``ValueError`` — see
        :func:`validate_policy` for the reasoning.

    Returns
    -------
    list[str]
        Absolute paths of files actually deleted. Empty if nothing to do.

    Raises
    ------
    ValueError
        Invalid policy combination (see :func:`validate_policy`).
    """
    validate_policy(keep_last_n, keep_best)

    m = load_manifest(checkpoint_dir)
    entries = m["entries"]
    if not entries:
        return []

    keep_indices = set()
    # preserve last n
    if keep_last_n is None:
        keep_indices.update(range(len(entries)))
    else:
        ordered_ = sorted(
            range(len(entries)),
            key=lambda i: (
                entries[i].get("step") if entries[i].get("step") is not None else -1,
                entries[i].get("ts", 0),
            ),
            reverse=True,
        )
        keep_indices.update(ordered_[:keep_last_n])
    # preserve best — most RECENT is_best entry (single-best invariant
    # means there should be at most one, but if older data has multiple
    # we explicitly pick the latest by index, which mirrors append order).
    if keep_best:
        bests = [i for i, e in enumerate(entries) if e.get("is_best", False)]
        if bests:
            keep_indices.add(bests[-1])
    # clean useless ckpt
    deleted = []
    for i, e in enumerate(entries):
        if i in keep_indices:
            continue
        abs_path = Path(os.path.join(checkpoint_dir, e["path"]))
        try:
            if abs_path.is_dir():
                shutil.rmtree(abs_path)
            elif abs_path.exists():
                os.unlink(abs_path)
            deleted.append(str(abs_path))
        except OSError as err:
            _LOG.warning(
                "runq cleanup: failed to delete %s: %s", abs_path, err,
            )
    # write back
    m["entries"] = [entries[i] for i in sorted(keep_indices)]
    save_manifest(checkpoint_dir, m)
    return deleted

