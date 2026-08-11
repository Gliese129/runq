"""Public utility helpers for user scripts.

Kept deliberately small: things user training scripts keep reinventing
(and getting subtly wrong), exposed under ``runq.utils``.
"""
from __future__ import annotations

import contextlib
import os
import tempfile
from pathlib import Path

__all__ = ["atomic_write"]


@contextlib.contextmanager
def atomic_write(path, mode: str = "w", encoding: str | None = None,
                 newline: str | None = None):
    """Write a file atomically: temp file in the same directory + rename.

    Readers (including a concurrent runq reap) never observe a partial
    file — they see either the previous content or the complete new one.
    If the body raises, the target is left untouched and the temp file
    is removed.

        with runq.utils.atomic_write("results.json") as f:
            json.dump(data, f)

        with runq.utils.atomic_write("blob.bin", mode="wb") as f:
            f.write(payload)

    Parameters
    ----------
    path :
        Target file path. The temp file is created in the SAME directory
        (os.replace is only atomic within one filesystem).
    mode :
        ``"w"`` (text, default) or ``"wb"`` (binary).
    encoding, newline :
        Text-mode options, same meaning as ``open``. Text mode defaults
        to UTF-8 (explicit, so the result doesn't depend on locale).
    """
    if mode not in ("w", "wb"):
        raise ValueError(
            f"atomic_write: mode must be 'w' or 'wb', got {mode!r}"
        )
    target = Path(path)
    binary = "b" in mode
    if not binary and encoding is None:
        encoding = "utf-8"

    tmp_dir = target.parent if str(target.parent) else Path(".")
    fd, tmp_name = tempfile.mkstemp(
        dir=tmp_dir, prefix=f".{target.name}.", suffix=".runq-tmp"
    )
    try:
        # mkstemp creates 0600; widen to the umask-honoring default so the
        # atomically-written file has the same perms a plain open() would.
        umask = os.umask(0)
        os.umask(umask)
        os.chmod(tmp_name, 0o666 & ~umask)

        if binary:
            f = os.fdopen(fd, mode)
        else:
            f = os.fdopen(fd, mode, encoding=encoding, newline=newline)
        with f:
            yield f
            # Flush + fsync BEFORE the rename: os.replace makes the name
            # switch atomic, but only fsync makes the content durable —
            # without it a crash can leave a complete-looking empty file.
            f.flush()
            os.fsync(f.fileno())
        os.replace(tmp_name, target)
    except BaseException:
        with contextlib.suppress(OSError):
            os.unlink(tmp_name)
        raise
