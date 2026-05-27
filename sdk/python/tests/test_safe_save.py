"""Tests for runq.safe_save (step 3: function form, no manifest, no walker).

What we're verifying:

- Path resolution: relative → ``ctx.checkpoint_dir``, absolute → as-is.
- Happy path: disk OK, file lands at final_path, checkpoint event in jsonl.
- TOCTOU defense: tmp file in same dir + atomic rename, no partial at
  ``final_path`` even if save_fn errors.
- Pre-flight freeze: low free + daemon mode → POST freeze-self → retry → succeed.
- ENOSPC mid-save: catch + clean tmp + freeze + retry.
- no_daemon mode: disk low → RunqDiskFullError (no daemon to call).
- manual mode: same RunqDiskFullError behavior.
- Checkpoint event format: path, size_bytes, step, is_best, ts, type=checkpoint.
- step=0 preserved (regression for Codex review #6 once more).
"""
from __future__ import annotations

import errno
import json
import os
import shutil
import tempfile
import threading
from pathlib import Path
from typing import Any
from unittest.mock import patch

import pytest

import runq
from runq._exceptions import RunqDiskFullError
from tests._fake_daemon import FakeDaemon


# ---- helpers ----

def _read_jsonl(path: Path) -> list[dict]:
    if not path.exists():
        return []
    return [json.loads(line) for line in path.read_text().splitlines() if line]


def _dummy_save(path: str, obj: Any) -> None:
    """Tiny save_fn for tests — writes repr(obj) so we can assert content."""
    Path(path).write_text(repr(obj))


def _disk_usage_seq(values: list[int]) -> Any:
    """Return a stub `disk_usage(path)` that walks the `values` sequence.

    Sticks on the last value once exhausted — implementations may
    legitimately call disk_usage more times than the test "logically"
    needs (e.g. re-checking free after an ENOSPC catch to report a
    fresher value to the daemon). Repeating the last value lets such
    designs pass without rewriting the test.

    If you genuinely need to assert a fixed number of calls, count them
    separately via a wrapper.
    """
    idx = [0]

    def stub(_path):
        i = min(idx[0], len(values) - 1)
        idx[0] += 1
        return shutil._ntuple_diskusage(1 << 50, 0, values[i])

    return stub


@pytest.fixture
def short_sock_path():
    """Short Unix socket path under /tmp (AF_UNIX 108-char limit)."""
    fd, path = tempfile.mkstemp(prefix="runq-t-", suffix=".sock", dir="/tmp")
    os.close(fd)
    os.unlink(path)
    yield path
    try:
        os.unlink(path)
    except FileNotFoundError:
        pass


@pytest.fixture
def daemon_ctx(clean_env, tmp_path, monkeypatch, short_sock_path):
    """Set up a daemon-mode context. Caller must start FakeDaemon manually."""
    monkeypatch.setenv("RUNQ_TASK_ID", "t1")
    monkeypatch.setenv("RUNQ_JOB_ID", "j1")
    monkeypatch.setenv("RUNQ_TASK_DIR", str(tmp_path))
    monkeypatch.setenv("RUNQ_METRICS_FILE", str(tmp_path / "metrics.jsonl"))
    monkeypatch.setenv("RUNQ_CHECKPOINT_DIR", str(tmp_path / "ckpts"))
    monkeypatch.setenv("RUNQ_SOCKET_PATH", short_sock_path)
    monkeypatch.setenv("RUNQ_SAFETY_FACTOR_PERCENT", "110")
    monkeypatch.setenv("RUNQ_SAFETY_EXTRA_GB", "0")
    ctx = runq.context()
    assert ctx.mode == "daemon"
    return ctx


@pytest.fixture
def no_daemon_ctx(clean_env, tmp_path, monkeypatch):
    monkeypatch.setenv("RUNQ_TASK_ID", "t1")
    monkeypatch.setenv("RUNQ_TASK_DIR", str(tmp_path))
    monkeypatch.setenv("RUNQ_METRICS_FILE", str(tmp_path / "metrics.jsonl"))
    monkeypatch.setenv("RUNQ_CHECKPOINT_DIR", str(tmp_path / "ckpts"))
    monkeypatch.setenv("RUNQ_NO_DAEMON", "1")
    ctx = runq.context()
    assert ctx.mode == "no_daemon"
    return ctx


# ---- path resolution ----

def test_relative_path_resolves_under_checkpoint_dir(daemon_ctx, monkeypatch):
    monkeypatch.setattr(shutil, "disk_usage", _disk_usage_seq([1 << 50]))
    runq.safe_save("ckpt.pt", {"k": "v"}, save_fn=_dummy_save, size_hint=100)
    target = daemon_ctx.checkpoint_dir / "ckpt.pt"
    assert target.exists(), f"expected save under {daemon_ctx.checkpoint_dir}"
    assert target.read_text() == "{'k': 'v'}"


def test_absolute_path_used_as_is(daemon_ctx, tmp_path, monkeypatch):
    monkeypatch.setattr(shutil, "disk_usage", _disk_usage_seq([1 << 50]))
    target = tmp_path / "elsewhere" / "x.pt"
    target.parent.mkdir(parents=True)
    runq.safe_save(str(target), {"x": 1}, save_fn=_dummy_save, size_hint=100)
    assert target.exists()


def test_relative_path_no_ckpt_dir_uses_cwd(clean_env, tmp_path, monkeypatch):
    """manual mode without RUNQ_CHECKPOINT_DIR → relative path lands in cwd."""
    monkeypatch.chdir(tmp_path)
    runq.context()
    monkeypatch.setattr(shutil, "disk_usage", _disk_usage_seq([1 << 50]))
    runq.safe_save("local.pt", "data", save_fn=_dummy_save, size_hint=100)
    assert (tmp_path / "local.pt").exists()


# ---- happy path ----

def test_successful_save_appends_checkpoint_event(daemon_ctx, monkeypatch):
    monkeypatch.setattr(shutil, "disk_usage", _disk_usage_seq([1 << 50]))
    runq.safe_save(
        "ckpt.pt", "data",
        save_fn=_dummy_save, step=5, is_best=True, size_hint=100,
    )
    events = _read_jsonl(daemon_ctx.metrics_file)
    ckpts = [e for e in events if e["type"] == "checkpoint"]
    assert len(ckpts) == 1
    e = ckpts[0]
    assert e["path"].endswith("ckpt.pt")
    assert e["step"] == 5
    assert e["is_best"] is True
    assert e["size_bytes"] > 0
    assert isinstance(e["ts"], int)


def test_step_zero_preserved_in_checkpoint_event(daemon_ctx, monkeypatch):
    """step=0 must be recorded as 0, not auto-replaced or treated as missing."""
    monkeypatch.setattr(shutil, "disk_usage", _disk_usage_seq([1 << 50]))
    runq.safe_save("ckpt.pt", "x", save_fn=_dummy_save, step=0, size_hint=100)
    events = _read_jsonl(daemon_ctx.metrics_file)
    ckpts = [e for e in events if e["type"] == "checkpoint"]
    assert ckpts[0]["step"] == 0


def test_no_tmp_file_after_success(daemon_ctx, monkeypatch):
    """After a successful save, no .runq-tmp-* file should linger."""
    monkeypatch.setattr(shutil, "disk_usage", _disk_usage_seq([1 << 50]))
    runq.safe_save("ckpt.pt", "x", save_fn=_dummy_save, size_hint=100)
    tmps = list(daemon_ctx.checkpoint_dir.glob("*.runq-tmp-*"))
    assert not tmps, f"tmp files left behind: {tmps}"


# ---- TOCTOU: pre-flight low + daemon mode ----

def test_preflight_low_triggers_freeze_then_retries(daemon_ctx, monkeypatch, short_sock_path):
    """Disk low on first check → POST freeze-self → daemon "thaws" → retry succeeds."""
    with FakeDaemon(short_sock_path) as fd:
        # 1st disk_usage: low. 2nd (after freeze): high.
        monkeypatch.setattr(shutil, "disk_usage", _disk_usage_seq([10, 1 << 50]))
        fd.queue_response(status=200, body={"frozen": True})

        runq.safe_save("ckpt.pt", "x", save_fn=_dummy_save, size_hint=100)

        # Verify daemon was contacted exactly once with the right body shape.
        assert len(fd.calls) == 1
        call_body = json.loads(fd.calls[0].body)
        assert call_body["task_id"] == "t1"
        assert call_body["free_bytes"] == 10
        # threshold = 100 * 110/100 + 0 = 110
        assert call_body["needed_est"] == 110
        # mount is the resolved mountpoint of the checkpoint dir; just
        # verify it's a non-empty path string.
        assert isinstance(call_body["mount"], str) and call_body["mount"]

    # Final file landed.
    assert (daemon_ctx.checkpoint_dir / "ckpt.pt").exists()


def test_preflight_low_no_daemon_raises(no_daemon_ctx, monkeypatch):
    monkeypatch.setattr(shutil, "disk_usage", _disk_usage_seq([10]))
    with pytest.raises(RunqDiskFullError) as ei:
        runq.safe_save("ckpt.pt", "x", save_fn=_dummy_save, size_hint=100)
    assert ei.value.free_bytes == 10
    assert ei.value.needed_bytes == 110
    assert ei.value.mount


# ---- TOCTOU: ENOSPC mid-save ----

def test_enospc_mid_save_catches_cleans_and_retries(daemon_ctx, monkeypatch, short_sock_path):
    """save_fn raises ENOSPC on attempt 1, succeeds on attempt 2.

    Verifies the catch + cleanup + freeze + retry path. The tmp file
    written before the ENOSPC should be removed; the final file should
    contain the data from the second (successful) call only.
    """
    with FakeDaemon(short_sock_path) as fd:
        # disk_usage always reports "plenty" — the ENOSPC comes from the
        # save_fn itself (simulating mid-write failure even though the
        # pre-check said OK). After freeze, second call has same "plenty"
        # available and succeeds.
        monkeypatch.setattr(shutil, "disk_usage", _disk_usage_seq([1 << 50, 1 << 50]))
        fd.queue_response(status=200, body={"frozen": True})

        call_count = [0]

        def flaky(path: str, obj: Any) -> None:
            call_count[0] += 1
            if call_count[0] == 1:
                # Write a partial then raise.
                Path(path).write_text("partial")
                raise OSError(errno.ENOSPC, "no space")
            Path(path).write_text("real")

        runq.safe_save("ckpt.pt", "x", save_fn=flaky, size_hint=100)

        assert call_count[0] == 2, "expected exactly one retry after ENOSPC"
        # Final file is the retry's content, not partial.
        assert (daemon_ctx.checkpoint_dir / "ckpt.pt").read_text() == "real"
        # No tmp file lingers.
        assert not list(daemon_ctx.checkpoint_dir.glob("*.runq-tmp-*"))


def test_enospc_no_daemon_raises_after_first_call(no_daemon_ctx, monkeypatch):
    """no_daemon mode + ENOSPC mid-save → tmp cleaned + RunqDiskFullError."""
    monkeypatch.setattr(shutil, "disk_usage", _disk_usage_seq([1 << 50]))

    def always_enospc(path: str, obj: Any) -> None:
        Path(path).write_text("partial")
        raise OSError(errno.ENOSPC, "no space")

    with pytest.raises(RunqDiskFullError):
        runq.safe_save("ckpt.pt", "x", save_fn=always_enospc, size_hint=100)

    # Critical: no tmp file left behind even on the error path.
    assert not list(no_daemon_ctx.checkpoint_dir.glob("*.runq-tmp-*"))


# ---- atomic rename invariant ----

def test_non_enospc_oserror_no_partial_at_final_path(daemon_ctx, monkeypatch):
    """If save_fn raises a non-ENOSPC OSError, final_path must not exist
    (the tmp file is where the partial would land, and we clean it).
    """
    monkeypatch.setattr(shutil, "disk_usage", _disk_usage_seq([1 << 50]))

    def bad(path: str, obj: Any) -> None:
        Path(path).write_text("would-be-partial")
        raise OSError(errno.EPERM, "permission denied")

    with pytest.raises(OSError) as ei:
        runq.safe_save("ckpt.pt", "x", save_fn=bad, size_hint=100)
    assert ei.value.errno == errno.EPERM

    # final_path should NOT exist (we never renamed).
    assert not (daemon_ctx.checkpoint_dir / "ckpt.pt").exists()
    # And tmp should be cleaned.
    assert not list(daemon_ctx.checkpoint_dir.glob("*.runq-tmp-*"))


def test_save_fn_writes_directly_to_final_path_would_break(daemon_ctx, monkeypatch):
    """Regression test: save_fn must receive a *tmp* path, not final_path.

    If safe_save accidentally passed final_path directly to save_fn, an
    ENOSPC mid-write would corrupt the final file. We verify save_fn
    is invoked with a path containing the tmp marker.
    """
    monkeypatch.setattr(shutil, "disk_usage", _disk_usage_seq([1 << 50]))
    received_path = []

    def record_path(path: str, obj: Any) -> None:
        received_path.append(path)
        Path(path).write_text("ok")

    runq.safe_save("ckpt.pt", "x", save_fn=record_path, size_hint=100)
    assert ".runq-tmp-" in received_path[0], (
        f"safe_save passed final_path directly to save_fn ({received_path[0]}); "
        "must pass a sibling tmp path so rename-after-write stays atomic"
    )


# ---- threshold math ----

def test_threshold_uses_safety_factor_and_extra_gb(clean_env, tmp_path, monkeypatch, short_sock_path):
    """needed_est = size_hint × factor / 100 + extra_gb × 1 GiB."""
    monkeypatch.setenv("RUNQ_TASK_ID", "t1")
    monkeypatch.setenv("RUNQ_TASK_DIR", str(tmp_path))
    monkeypatch.setenv("RUNQ_METRICS_FILE", str(tmp_path / "m.jsonl"))
    monkeypatch.setenv("RUNQ_CHECKPOINT_DIR", str(tmp_path / "ckpts"))
    monkeypatch.setenv("RUNQ_SOCKET_PATH", short_sock_path)
    monkeypatch.setenv("RUNQ_SAFETY_FACTOR_PERCENT", "150")
    monkeypatch.setenv("RUNQ_SAFETY_EXTRA_GB", "2")
    runq.context()

    with FakeDaemon(short_sock_path) as fd:
        monkeypatch.setattr(shutil, "disk_usage", _disk_usage_seq([10, 1 << 60]))
        fd.queue_response(status=200, body={"frozen": True})

        runq.safe_save("ckpt.pt", "x", save_fn=_dummy_save, size_hint=1000)

        body = json.loads(fd.calls[0].body)
        # 1000 × 150/100 + 2 × 1GiB = 1500 + 2 * (1<<30)
        assert body["needed_est"] == 1500 + 2 * (1 << 30)
