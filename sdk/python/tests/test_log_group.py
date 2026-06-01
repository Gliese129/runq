"""Tests for runq.log_group — dual context-manager / decorator metric prefix.

Contract:
- ``with log_group("train"):`` and ``@log_group("train")`` both push
  the prefix; both pop on exit (CM exit / wrapped fn return).
- Nesting joins with '/': log_group("a") inside log_group("b") →
  prefix "b/a".
- log_metric, report's emitted key, and @epoch's
  "epoch_time_seconds" all get the prefix.
- report's HISTORY entries keep the unprefixed key names (hooks
  shouldn't have to know about log_group).
- Exception inside the CM/decorator still pops the prefix.
- Validation: prefix must be a non-empty string, no '/' inside.
- Backed by contextvars — concurrent tasks don't bleed prefixes.
"""
import json
import threading

import pytest

import runq
from runq._report import _get_history_for_tests


def _read_events(path):
    return [json.loads(line) for line in path.read_text().splitlines()]


# ---- validation ----

def test_log_group_rejects_empty_prefix(clean_env, tmp_path, monkeypatch):
    with pytest.raises(ValueError, match="non-empty"):
        runq.log_group("")


def test_log_group_rejects_slash_in_prefix(clean_env, tmp_path, monkeypatch):
    with pytest.raises(ValueError, match="must not contain"):
        runq.log_group("train/loss")


def test_log_group_rejects_non_str_prefix(clean_env, tmp_path, monkeypatch):
    with pytest.raises(TypeError, match="string"):
        runq.log_group(42)   # type: ignore[arg-type]


# ---- context-manager form ----

def test_log_group_cm_prefixes_log_metric(clean_env, tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    runq.context()
    with runq.log_group("train"):
        runq.log_metric("loss", 0.5, step=1)
    events = _read_events(tmp_path / "runq_metrics.jsonl")
    assert events[0]["key"] == "train/loss"


def test_log_group_cm_pops_prefix_on_exit(clean_env, tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    runq.context()
    with runq.log_group("train"):
        runq.log_metric("loss", 0.5, step=1)
    runq.log_metric("untagged", 9.0, step=2)
    events = _read_events(tmp_path / "runq_metrics.jsonl")
    keys = [e["key"] for e in events]
    assert keys == ["train/loss", "untagged"]


def test_log_group_cm_pops_prefix_after_exception(clean_env, tmp_path, monkeypatch):
    """Prefix must be popped even if user code raises inside the CM."""
    monkeypatch.chdir(tmp_path)
    runq.context()
    try:
        with runq.log_group("train"):
            raise RuntimeError("user fn crashed")
    except RuntimeError:
        pass

    runq.log_metric("after_crash", 1.0, step=1)
    events = _read_events(tmp_path / "runq_metrics.jsonl")
    assert events[0]["key"] == "after_crash"   # NOT "train/after_crash"


def test_log_group_cm_nesting(clean_env, tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    runq.context()
    with runq.log_group("outer"):
        runq.log_metric("a", 1.0, step=0)
        with runq.log_group("inner"):
            runq.log_metric("b", 2.0, step=1)
        runq.log_metric("c", 3.0, step=2)
    events = _read_events(tmp_path / "runq_metrics.jsonl")
    assert [e["key"] for e in events] == ["outer/a", "outer/inner/b", "outer/c"]


# ---- decorator form ----

def test_log_group_decorator_prefixes_log_metric(clean_env, tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    runq.context()

    @runq.log_group("train")
    def step():
        runq.log_metric("loss", 0.5, step=1)

    step()
    events = _read_events(tmp_path / "runq_metrics.jsonl")
    assert events[0]["key"] == "train/loss"


def test_log_group_decorator_pops_after_return(clean_env, tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    runq.context()

    @runq.log_group("train")
    def step():
        runq.log_metric("loss", 0.5, step=1)

    step()
    runq.log_metric("after", 1.0, step=2)
    events = _read_events(tmp_path / "runq_metrics.jsonl")
    assert [e["key"] for e in events] == ["train/loss", "after"]


def test_log_group_decorator_pops_after_exception(clean_env, tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    runq.context()

    @runq.log_group("train")
    def crash():
        runq.log_metric("before_crash", 1.0, step=1)
        raise RuntimeError("boom")

    with pytest.raises(RuntimeError):
        crash()
    runq.log_metric("after", 2.0, step=2)
    events = _read_events(tmp_path / "runq_metrics.jsonl")
    keys = [e["key"] for e in events]
    assert keys == ["train/before_crash", "after"]


def test_log_group_decorator_preserves_fn_metadata(clean_env):
    @runq.log_group("train")
    def my_step():
        """My docstring."""
        pass

    assert my_step.__name__ == "my_step"
    assert my_step.__doc__ == "My docstring."


# ---- report() prefixing ----

def test_log_group_applies_to_report(clean_env, tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    runq.context()
    with runq.log_group("train"):
        runq.report({"loss": 0.5, "acc": 0.9}, step=1)
    events = _read_events(tmp_path / "runq_metrics.jsonl")
    keys = sorted(e["key"] for e in events)
    assert keys == ["train/acc", "train/loss"]


def test_log_group_report_history_keeps_unprefixed_keys(clean_env, tmp_path, monkeypatch):
    """Hooks see clean key names; prefix is an emission-only concept."""
    monkeypatch.chdir(tmp_path)
    runq.context()
    with runq.log_group("train"):
        runq.report({"loss": 0.5}, step=1)
    history = _get_history_for_tests()
    assert "loss" in history[0]["metrics"]
    assert "train/loss" not in history[0]["metrics"]


# ---- thread isolation via contextvars ----

def test_log_group_thread_isolation(clean_env, tmp_path, monkeypatch):
    """Two threads with their own log_group blocks don't bleed into each other.

    Each thread's ContextVar copy starts empty in the new thread.
    Reading the prefix from inside a `with log_group("X")` on thread A
    must not see thread B's prefix.
    """
    monkeypatch.chdir(tmp_path)
    runq.context()
    from runq._prefix import current_prefix

    seen = {}
    barrier = threading.Barrier(2)

    def worker(tag):
        with runq.log_group(tag):
            barrier.wait()    # both threads hold their group at the same time
            seen[tag] = current_prefix()

    t1 = threading.Thread(target=worker, args=("a",))
    t2 = threading.Thread(target=worker, args=("b",))
    t1.start()
    t2.start()
    t1.join()
    t2.join()

    # Each thread sees ONLY its own prefix — no cross-pollination.
    assert seen == {"a": "a", "b": "b"}
