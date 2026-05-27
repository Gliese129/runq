"""Integration tests for step 5 — safe_save + manifest + cleanup.

These tests drive `safe_save` end-to-end and assert what's on disk
afterwards. The unit-level manifest invariants live in
`test_manifest.py`.

Covered surfaces:
- Parameterized decorator `@runq.safe_save(keep_last_n=N, keep_best=B)`
  dispatches correctly (returns a callable that itself returns nothing).
- Every successful save adds a manifest entry; key is relative to
  ctx.checkpoint_dir.
- `keep_last_n` deletes only the oldest tracked checkpoints.
- `keep_best=True` rescues the is_best entry across cleanup runs.
- Saves landing OUTSIDE ctx.checkpoint_dir are not tracked
  (no manifest entry, no cleanup applies to them).
- Untracked sibling files in ctx.checkpoint_dir survive cleanup.
"""
import json
import os
import shutil
import tempfile
from pathlib import Path

import pytest

import runq
from runq import _manifest


# ---- fixtures ----

def _disk_usage_seq(values):
    """Sticky-on-last so freeze-retry tests work without re-stubbing."""
    idx = [0]

    def stub(_path):
        i = min(idx[0], len(values) - 1)
        idx[0] += 1
        return shutil._ntuple_diskusage(1 << 50, 0, values[i])

    return stub


@pytest.fixture
def daemon_ctx(clean_env, tmp_path, monkeypatch):
    monkeypatch.setenv("RUNQ_TASK_ID", "t1")
    monkeypatch.setenv("RUNQ_JOB_ID", "j1")
    monkeypatch.setenv("RUNQ_TASK_DIR", str(tmp_path))
    monkeypatch.setenv("RUNQ_METRICS_FILE", str(tmp_path / "metrics.jsonl"))
    monkeypatch.setenv("RUNQ_CHECKPOINT_DIR", str(tmp_path / "ckpts"))
    monkeypatch.setenv("RUNQ_NO_DAEMON", "1")  # avoid socket setup for cleanup tests
    return runq.context()


def _names_in(checkpoint_dir):
    """Sorted .pt filenames in the directory."""
    return sorted(
        p.name for p in Path(checkpoint_dir).iterdir() if p.suffix == ".pt"
    )


def _load_manifest(checkpoint_dir):
    return _manifest.load_manifest(checkpoint_dir)


# ---- parameterized decorator dispatch -----------------------------

def test_parameterized_decorator_returns_factory_then_wrapper(daemon_ctx):
    """`safe_save(...)` with no positional → factory; factory(fn) → wrapper."""
    factory = runq.safe_save(keep_last_n=3)
    assert callable(factory)

    def raw(path, data):
        Path(path).write_text(data)

    wrapped = factory(raw)
    assert callable(wrapped)
    # __name__ preserved via functools.wraps
    assert wrapped.__name__ == "raw"


def test_parameterized_decorator_at_syntax(daemon_ctx, monkeypatch):
    """`@safe_save(keep_last_n=3)` is the user-facing spelling."""
    monkeypatch.setattr(shutil, "disk_usage", _disk_usage_seq([1 << 50]))

    @runq.safe_save(keep_last_n=3)
    def my_save(path, data):
        Path(path).write_text(data)

    my_save("ckpt.pt", "hello", size_hint=100)
    assert (daemon_ctx.checkpoint_dir / "ckpt.pt").read_text() == "hello"


def test_parameterized_decorator_on_non_callable_raises(daemon_ctx):
    factory = runq.safe_save(keep_last_n=3)
    with pytest.raises(TypeError, match="decorator"):
        factory("not a function")  # type: ignore[arg-type]


def test_parameterized_decorator_keep_best_without_n_raises_at_construct(daemon_ctx):
    """@safe_save(keep_best=True) (no N) must fail at decoration time,
    NOT inside the user's first call."""
    with pytest.raises(ValueError, match="keep_last_n"):
        runq.safe_save(keep_best=True)


def test_function_form_keep_best_without_n_raises(daemon_ctx, monkeypatch):
    monkeypatch.setattr(shutil, "disk_usage", _disk_usage_seq([1 << 50]))
    with pytest.raises(ValueError, match="keep_last_n"):
        runq.safe_save(
            "ckpt.pt", "data",
            save_fn=lambda p, o: Path(p).write_text(o),
            size_hint=100,
            keep_best=True,  # no keep_last_n
        )


def test_function_form_negative_n_raises(daemon_ctx, monkeypatch):
    monkeypatch.setattr(shutil, "disk_usage", _disk_usage_seq([1 << 50]))
    with pytest.raises(ValueError, match="keep_last_n must be >= 0"):
        runq.safe_save(
            "ckpt.pt", "data",
            save_fn=lambda p, o: Path(p).write_text(o),
            size_hint=100,
            keep_last_n=-1,
        )


def test_decorator_n_zero_keep_best_keeps_only_best(daemon_ctx, monkeypatch):
    """End-to-end test of the user-explicit 'best only' contract."""
    monkeypatch.setattr(shutil, "disk_usage", _disk_usage_seq([1 << 50]))

    @runq.safe_save(keep_last_n=0, keep_best=True)
    def my_save(path, data):
        Path(path).write_text(data)

    my_save("first.pt", "d", size_hint=100, step=1)
    my_save("the_best.pt", "d", size_hint=100, step=2, is_best=True)
    my_save("after.pt", "d", size_hint=100, step=3)

    # Only the best survives — N=0 evicts every non-best.
    assert _names_in(daemon_ctx.checkpoint_dir) == ["the_best.pt"]


# ---- manifest entries appear after save ---------------------------

def test_function_form_appends_manifest_entry(daemon_ctx, monkeypatch):
    monkeypatch.setattr(shutil, "disk_usage", _disk_usage_seq([1 << 50]))

    def save_fn(path, obj):
        Path(path).write_text(obj)

    runq.safe_save(
        "ckpt-1.pt", "data", save_fn=save_fn, size_hint=100, step=1, is_best=True
    )
    m = _load_manifest(daemon_ctx.checkpoint_dir)
    assert len(m["entries"]) == 1
    e = m["entries"][0]
    assert e["path"] == "ckpt-1.pt"
    assert e["step"] == 1
    assert e["is_best"] is True
    assert e["size_bytes"] == len("data")


def test_decorator_form_appends_manifest_entry(daemon_ctx, monkeypatch):
    monkeypatch.setattr(shutil, "disk_usage", _disk_usage_seq([1 << 50]))

    @runq.safe_save
    def my_save(path, data):
        Path(path).write_text(data)

    my_save("ckpt-1.pt", "data", size_hint=100, step=1, is_best=True)
    m = _load_manifest(daemon_ctx.checkpoint_dir)
    assert len(m["entries"]) == 1
    assert m["entries"][0]["saved_by"] == "my_save"


# ---- keep_last_n + keep_best end-to-end ---------------------------

def test_keep_last_n_via_decorator(daemon_ctx, monkeypatch):
    monkeypatch.setattr(shutil, "disk_usage", _disk_usage_seq([1 << 50]))

    @runq.safe_save(keep_last_n=2)
    def my_save(path, data):
        Path(path).write_text(data)

    for i in range(5):
        my_save(f"ck{i}.pt", f"d{i}", size_hint=100, step=i)

    # Only the 2 newest survive on disk.
    assert _names_in(daemon_ctx.checkpoint_dir) == ["ck3.pt", "ck4.pt"]


def test_keep_best_via_decorator(daemon_ctx, monkeypatch):
    monkeypatch.setattr(shutil, "disk_usage", _disk_usage_seq([1 << 50]))

    @runq.safe_save(keep_last_n=2, keep_best=True)
    def my_save(path, data):
        Path(path).write_text(data)

    my_save("ck0.pt", "d", size_hint=100, step=0, is_best=True)  # is the best
    my_save("ck1.pt", "d", size_hint=100, step=1)
    my_save("ck2.pt", "d", size_hint=100, step=2)
    my_save("ck3.pt", "d", size_hint=100, step=3)

    # keep_last_n=2 → ck2, ck3. keep_best rescues ck0 even though it's step 0.
    assert _names_in(daemon_ctx.checkpoint_dir) == ["ck0.pt", "ck2.pt", "ck3.pt"]


def test_keep_best_demotion_after_new_best(daemon_ctx, monkeypatch):
    """Two saves with is_best=True at different times — the older one
    loses its flag (single-best invariant), so keep_best follows the
    *current* best, not the historical one."""
    monkeypatch.setattr(shutil, "disk_usage", _disk_usage_seq([1 << 50]))

    @runq.safe_save(keep_last_n=1, keep_best=True)
    def my_save(path, data):
        Path(path).write_text(data)

    my_save("old_best.pt", "d", size_hint=100, step=1, is_best=True)
    my_save("filler.pt", "d", size_hint=100, step=2)
    my_save("new_best.pt", "d", size_hint=100, step=3, is_best=True)
    my_save("latest.pt", "d", size_hint=100, step=4)

    # keep_last_n=1 → latest.pt. keep_best → new_best.pt (current best).
    # old_best is no longer flagged best → evicted.
    assert _names_in(daemon_ctx.checkpoint_dir) == ["latest.pt", "new_best.pt"]


# ---- safety invariants --------------------------------------------

def test_save_outside_checkpoint_dir_not_tracked(daemon_ctx, monkeypatch, tmp_path):
    """Absolute path outside ctx.checkpoint_dir → no manifest entry."""
    monkeypatch.setattr(shutil, "disk_usage", _disk_usage_seq([1 << 50]))
    outside = tmp_path / "elsewhere" / "model.pt"
    outside.parent.mkdir()

    def save_fn(path, obj):
        Path(path).write_text(obj)

    runq.safe_save(str(outside), "hi", save_fn=save_fn, size_hint=100, step=1)
    assert outside.exists()

    # checkpoint_dir manifest stays empty.
    m = _load_manifest(daemon_ctx.checkpoint_dir)
    assert m["entries"] == []


def test_cleanup_does_not_touch_user_siblings(daemon_ctx, monkeypatch):
    """The integration-level smoke test for manifest-scoped delete."""
    monkeypatch.setattr(shutil, "disk_usage", _disk_usage_seq([1 << 50]))

    @runq.safe_save(keep_last_n=1)
    def my_save(path, data):
        Path(path).write_text(data)

    # User puts a manual file in the dir BEFORE any runq save.
    daemon_ctx.checkpoint_dir.mkdir(parents=True, exist_ok=True)
    (daemon_ctx.checkpoint_dir / "user_config.yaml").write_text("lr: 1e-4")
    (daemon_ctx.checkpoint_dir / "manual.pt").write_text("hand-saved by user")

    # Now several runq saves — most will be evicted by keep_last_n=1.
    for i in range(4):
        my_save(f"ck{i}.pt", "d", size_hint=100, step=i)

    # Last runq ckpt + both user files survive.
    surviving = sorted(p.name for p in daemon_ctx.checkpoint_dir.iterdir())
    assert "user_config.yaml" in surviving
    assert "manual.pt" in surviving
    assert "ck3.pt" in surviving
    # Older runq ckpts are gone.
    for older in ["ck0.pt", "ck1.pt", "ck2.pt"]:
        assert older not in surviving, f"{older} should have been cleaned"


def test_no_policy_means_no_cleanup(daemon_ctx, monkeypatch):
    """Bare decorator (no keep_*) → manifest still tracks, but nothing deleted."""
    monkeypatch.setattr(shutil, "disk_usage", _disk_usage_seq([1 << 50]))

    @runq.safe_save
    def my_save(path, data):
        Path(path).write_text(data)

    for i in range(4):
        my_save(f"ck{i}.pt", "d", size_hint=100, step=i)

    # All four still on disk.
    assert _names_in(daemon_ctx.checkpoint_dir) == ["ck0.pt", "ck1.pt", "ck2.pt", "ck3.pt"]
    # Manifest tracked them all.
    m = _load_manifest(daemon_ctx.checkpoint_dir)
    assert [e["path"] for e in m["entries"]] == ["ck0.pt", "ck1.pt", "ck2.pt", "ck3.pt"]
