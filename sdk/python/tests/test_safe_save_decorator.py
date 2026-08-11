"""Tests for ``@runq.safe_save`` decorator form (step 4).

Decorator form lets the user write their own save fn (multiple files,
HuggingFace ``save_pretrained``, whatever) while still getting the
TOCTOU + freeze guards. SDK intercepts ``step`` / ``is_best`` /
``size_hint`` kwargs and either forwards or strips them depending on
the user fn's signature.

These tests cover the decorator-specific behavior (dispatch, kwarg
strip, auto-estimate). The underlying TOCTOU + freeze flow is already
covered by ``test_safe_save.py``.
"""
import errno
import json
import os
import shutil
import tempfile
from pathlib import Path
from typing import Any

import pytest

import runq
from runq._exceptions import RunqDiskFullError
from tests._fake_daemon import FakeDaemon

# ---- shared fixtures (mirror test_safe_save.py) ----

def _disk_usage_seq(values):
    idx = [0]

    def stub(_path):
        i = min(idx[0], len(values) - 1)
        idx[0] += 1
        return shutil._ntuple_diskusage(1 << 50, 0, values[i])

    return stub


@pytest.fixture
def short_sock_path():
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
    monkeypatch.setenv("RUNQ_TASK_ID", "t1")
    monkeypatch.setenv("RUNQ_JOB_ID", "j1")
    monkeypatch.setenv("RUNQ_TASK_DIR", str(tmp_path))
    monkeypatch.setenv("RUNQ_METRICS_FILE", str(tmp_path / "metrics.jsonl"))
    monkeypatch.setenv("RUNQ_CHECKPOINT_DIR", str(tmp_path / "ckpts"))
    monkeypatch.setenv("RUNQ_SOCKET_PATH", short_sock_path)
    ctx = runq.context()
    return ctx


def _read_jsonl(path: Path) -> list[dict]:
    if not path.exists():
        return []
    return [json.loads(line) for line in path.read_text().splitlines() if line]


# ---- dispatch ----

def test_decorator_bare_returns_wrapper(daemon_ctx, monkeypatch):
    """``@runq.safe_save`` (no parens) wraps the function."""
    monkeypatch.setattr(shutil, "disk_usage", _disk_usage_seq([1 << 50]))

    @runq.safe_save
    def my_save(path, data):
        Path(path).write_text(data)

    assert callable(my_save)
    my_save("ckpt.pt", "hello", size_hint=100)
    assert (daemon_ctx.checkpoint_dir / "ckpt.pt").read_text() == "hello"


def test_decorator_with_positional_obj_raises(clean_env):
    """``runq.safe_save(some_fn, other_arg)`` is ambiguous → TypeError."""
    def _save(p, d): pass
    with pytest.raises(TypeError, match="@decorator"):
        runq.safe_save(_save, "extra_arg")  # type: ignore[call-overload]


# ---- runq-managed kwargs: strip vs forward ----

def test_step_stripped_when_user_fn_doesnt_declare(daemon_ctx, monkeypatch):
    """User fn doesn't declare `step` → SDK strips it before forwarding."""
    monkeypatch.setattr(shutil, "disk_usage", _disk_usage_seq([1 << 50]))
    received_kwargs: list[dict] = []

    @runq.safe_save
    def my_save(path, data, **kwargs):
        received_kwargs.append(kwargs)
        Path(path).write_text(data)

    my_save("ckpt.pt", "x", step=5, is_best=True, size_hint=100)

    # User fn shouldn't see step / is_best / size_hint (none declared).
    assert "step" not in received_kwargs[0]
    assert "is_best" not in received_kwargs[0]
    assert "size_hint" not in received_kwargs[0]

    # But SDK should still record step + is_best in the checkpoint event.
    events = _read_jsonl(daemon_ctx.events_file)
    ckpts = [e for e in events if e["type"] == "checkpoint"]
    assert ckpts[0]["step"] == 5
    assert ckpts[0]["is_best"] is True


def test_step_forwarded_when_user_fn_declares_it(daemon_ctx, monkeypatch):
    """User fn declares `step` in its signature → SDK forwards it."""
    monkeypatch.setattr(shutil, "disk_usage", _disk_usage_seq([1 << 50]))
    received_step: list = []

    @runq.safe_save
    def my_save(path, data, step):
        received_step.append(step)
        Path(path).write_text(f"{data}@{step}")

    my_save("ckpt.pt", "x", step=7, size_hint=100)

    assert received_step == [7]
    # SDK also recorded it.
    events = _read_jsonl(daemon_ctx.events_file)
    assert next(e for e in events if e["type"] == "checkpoint")["step"] == 7


def test_user_kwargs_preserved(daemon_ctx, monkeypatch):
    """User's own kwargs (lr, schedule, etc.) pass through untouched."""
    monkeypatch.setattr(shutil, "disk_usage", _disk_usage_seq([1 << 50]))
    received: list = []

    @runq.safe_save
    def my_save(path, model, optim, lr=None):
        received.append((model, optim, lr))
        Path(path).write_text("ok")

    my_save("ckpt.pt", "model-obj", "optim-obj", lr=1e-4, size_hint=100)

    assert received == [("model-obj", "optim-obj", 1e-4)]


# ---- size auto-estimation ----

def test_size_hint_explicit_skips_walker(daemon_ctx, monkeypatch):
    """Explicit size_hint should be used verbatim — no walker call."""
    monkeypatch.setattr(shutil, "disk_usage", _disk_usage_seq([1 << 50]))

    @runq.safe_save
    def my_save(path, data):
        Path(path).write_text(data)

    # Pass size_hint explicitly even though data isn't a tensor.
    my_save("ckpt.pt", "x", size_hint=42)
    events = _read_jsonl(daemon_ctx.events_file)
    assert events[0]["type"] == "checkpoint"


def test_no_size_hint_no_torch_raises_helpful_error(daemon_ctx, monkeypatch):
    """size_hint=None + walker found nothing → TypeError directing user
    to pass size_hint or install torch."""
    monkeypatch.setattr(shutil, "disk_usage", _disk_usage_seq([1 << 50]))

    @runq.safe_save
    def my_save(path, data):
        Path(path).write_text(data)

    with pytest.raises(TypeError, match=r"size_hint=N|install torch"):
        my_save("ckpt.pt", "a non-tensor string")


def test_size_hint_auto_estimate_from_tensor(daemon_ctx, monkeypatch):
    """When user passes a torch.Tensor arg and no size_hint, walker computes it."""
    torch = pytest.importorskip("torch")
    monkeypatch.setattr(shutil, "disk_usage", _disk_usage_seq([1 << 50]))

    @runq.safe_save
    def my_save(path, tensor):
        Path(path).write_bytes(b"x" * 100)

    t = torch.zeros(100, dtype=torch.float32)
    my_save("ckpt.pt", t)  # no size_hint
    # Should succeed — size walker estimated from the tensor.
    assert (daemon_ctx.checkpoint_dir / "ckpt.pt").exists()


# ---- TOCTOU integration ----

def test_decorator_path_resolution_relative(daemon_ctx, monkeypatch):
    """Relative path passes through to function-form path resolution.

    Uses a flat filename — nested subdirs require parent mkdir, which is
    out of scope for step 3's safe_save body (no auto-mkdir promised).
    Users who want subdirs either mkdir manually or wait for a future
    step that adds parent creation.
    """
    monkeypatch.setattr(shutil, "disk_usage", _disk_usage_seq([1 << 50]))

    @runq.safe_save
    def my_save(path, data):
        Path(path).write_text(data)

    my_save("ckpt-via-decorator.pt", "hello", size_hint=100)
    target = daemon_ctx.checkpoint_dir / "ckpt-via-decorator.pt"
    assert target.exists()
    assert target.read_text() == "hello"


def test_decorator_freeze_round_trip(daemon_ctx, monkeypatch, short_sock_path):
    """Decorator form should also exercise the freeze flow."""
    with FakeDaemon(short_sock_path) as fd:
        monkeypatch.setattr(shutil, "disk_usage", _disk_usage_seq([10, 1 << 50]))
        fd.queue_response(status=200, body={"frozen": True})

        @runq.safe_save
        def my_save(path, data):
            Path(path).write_text(data)

        my_save("ckpt.pt", "x", size_hint=100)

        # Daemon got the freeze-self call.
        assert len(fd.calls) == 1
        body = json.loads(fd.calls[0].body)
        assert body["task_id"] == "t1"
        assert body["free_bytes"] == 10
