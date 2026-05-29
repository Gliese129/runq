"""Tests for @runq.epoch — pure timing decorator.

Contract recap (Codex review #7):
- Wraps a fn with time.time() before/after; emits one
  "epoch_time_seconds" metric event on completion.
- Does NOT touch step (step ownership is report/safe_save).
- Composes with @log_group (prefix applies to the emitted key).
- Even on user fn raising, the timing event still gets emitted
  (try/finally) — useful for "did it OOM after 5min or 5hr".
"""
import json
import time

import pytest

import runq


def _read_events(path):
    return [json.loads(line) for line in path.read_text().splitlines()]


# ---- basic behavior ----

def test_epoch_emits_one_metric_event(clean_env, tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    runq.context()

    @runq.epoch
    def train():
        time.sleep(0.01)
        return "ok"

    result = train()
    assert result == "ok"

    events = _read_events(tmp_path / "runq_metrics.jsonl")
    assert len(events) == 1
    e = events[0]
    assert e["type"] == "metric"
    assert e["key"] == "epoch_time_seconds"
    assert isinstance(e["value"], float)
    assert e["value"] >= 0.01
    # No step concept — must write null.
    assert e["step"] is None


def test_epoch_preserves_fn_metadata(clean_env, tmp_path, monkeypatch):
    """functools.wraps means introspection still works."""
    monkeypatch.chdir(tmp_path)
    runq.context()

    @runq.epoch
    def my_train_fn():
        """My docstring."""
        pass

    assert my_train_fn.__name__ == "my_train_fn"
    assert my_train_fn.__doc__ == "My docstring."


def test_epoch_emits_even_when_fn_raises(clean_env, tmp_path, monkeypatch):
    """Wall-clock is still useful after a crash. Don't swallow the exc."""
    monkeypatch.chdir(tmp_path)
    runq.context()

    @runq.epoch
    def boom():
        time.sleep(0.005)
        raise RuntimeError("training OOMed")

    with pytest.raises(RuntimeError, match="OOMed"):
        boom()

    events = _read_events(tmp_path / "runq_metrics.jsonl")
    assert len(events) == 1
    assert events[0]["key"] == "epoch_time_seconds"
    assert events[0]["value"] >= 0.005


def test_epoch_passes_through_args_and_return(clean_env, tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    runq.context()

    @runq.epoch
    def add(a, b, scale=1):
        return (a + b) * scale

    assert add(2, 3, scale=10) == 50


def test_epoch_does_not_touch_step(clean_env, tmp_path, monkeypatch):
    """@epoch must NOT write/advance ctx.current_step."""
    monkeypatch.chdir(tmp_path)
    ctx = runq.context()
    ctx.current_step = 42  # set externally; @epoch must not disturb it

    @runq.epoch
    def train():
        pass

    train()
    assert ctx.current_step == 42


def test_epoch_multiple_invocations_emit_multiple_events(clean_env, tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    runq.context()

    @runq.epoch
    def step():
        time.sleep(0.005)

    for _ in range(3):
        step()

    events = _read_events(tmp_path / "runq_metrics.jsonl")
    assert len(events) == 3
    assert all(e["key"] == "epoch_time_seconds" for e in events)
