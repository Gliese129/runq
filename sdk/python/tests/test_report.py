"""Tests for runq.report — Decision dataclass, jsonl emission, step writeback.

The hook-orchestration tests live in `test_early_stop.py`. These tests
focus on report()'s mechanical contract: writes metrics to jsonl,
appends to history, maintains ctx.current_step.

Most tests here run with ZERO hooks registered — they exercise only
the no-hook path of `_run_early_stop_hooks`, which still has to
return a non-stop Decision. The hook-core tests in
test_early_stop.py exercise registration + short-circuit.
"""
import json
from dataclasses import FrozenInstanceError

import pytest

import runq
from runq._report import _get_history_for_tests


def _read_events(path):
    return [json.loads(line) for line in path.read_text().splitlines()]


# ---- Decision dataclass shape ------------------------------------

def test_decision_frozen_default(clean_env, tmp_path, monkeypatch):
    """Decision is a frozen dataclass — mutation raises FrozenInstanceError."""
    monkeypatch.chdir(tmp_path)
    runq.context()
    d = runq.report({"loss": 0.5})
    assert isinstance(d, runq.Decision)
    assert d.should_stop is False
    assert d.reason is None
    with pytest.raises(FrozenInstanceError):
        d.should_stop = True  # type: ignore[misc]


def test_decision_default_fields():
    """Decision() should be constructable with no args (sane defaults)."""
    d = runq.Decision()
    assert d.should_stop is False
    assert d.reason is None


# ---- report() basic mechanics ------------------------------------

def test_report_single_metric_writes_one_event(clean_env, tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    runq.context()
    runq.report({"loss": 0.42}, step=5)
    events = _read_events(tmp_path / "runq_metrics.jsonl")
    assert len(events) == 1
    assert events[0]["type"] == "metric"
    assert events[0]["key"] == "loss"
    assert events[0]["value"] == 0.42
    assert events[0]["step"] == 5


def test_report_multiple_metrics_writes_one_event_each(clean_env, tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    runq.context()
    runq.report({"loss": 0.5, "acc": 0.9, "lr": 1e-4}, step=3)
    events = _read_events(tmp_path / "runq_metrics.jsonl")
    assert len(events) == 3
    keys = sorted(e["key"] for e in events)
    assert keys == ["acc", "loss", "lr"]
    # All share the same step + ts (one report → one timestamp).
    assert {e["step"] for e in events} == {3}
    assert len({e["ts"] for e in events}) == 1


def test_report_empty_metrics_writes_no_events(clean_env, tmp_path, monkeypatch):
    """report({}) is a valid 'just poll the hooks' call."""
    monkeypatch.chdir(tmp_path)
    runq.context()
    d = runq.report({})
    assert d.should_stop is False
    metrics_file = tmp_path / "runq_metrics.jsonl"
    # Either no file or zero events.
    if metrics_file.exists():
        assert metrics_file.read_text() == ""


def test_report_non_dict_raises(clean_env, tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    runq.context()
    with pytest.raises(TypeError, match="must be a dict"):
        runq.report(["loss", 0.5])  # type: ignore[arg-type]


# ---- step semantics: writeback + fallback ------------------------

def test_report_explicit_step_writes_back_to_ctx(clean_env, tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    ctx = runq.context()
    runq.report({"loss": 0.5}, step=7)
    assert ctx.current_step == 7


def test_report_step_none_uses_ctx_fallback(clean_env, tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    runq.context()
    runq.report({"a": 1.0}, step=4)
    runq.report({"a": 2.0})  # no step → falls back to ctx (4)
    events = _read_events(tmp_path / "runq_metrics.jsonl")
    assert [e["step"] for e in events] == [4, 4]


def test_report_step_zero_preserved(clean_env, tmp_path, monkeypatch):
    """step=0 is legal — does NOT fall back to ctx."""
    monkeypatch.chdir(tmp_path)
    ctx = runq.context()
    ctx.current_step = 99  # noise that fallback would pick up if step=0 misread
    runq.report({"loss": 0.5}, step=0)
    events = _read_events(tmp_path / "runq_metrics.jsonl")
    assert events[0]["step"] == 0
    assert ctx.current_step == 0  # writeback also went through


def test_report_step_none_with_unset_ctx_writes_null(clean_env, tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    runq.context()
    runq.report({"loss": 0.5})
    events = _read_events(tmp_path / "runq_metrics.jsonl")
    assert events[0]["step"] is None


def test_report_and_log_metric_share_step_view(clean_env, tmp_path, monkeypatch):
    """Both helpers read+write the same ctx.current_step."""
    monkeypatch.chdir(tmp_path)
    runq.context()
    runq.report({"a": 1.0}, step=10)
    runq.log_metric("b", 2.0)        # uses ctx (10)
    runq.log_metric("c", 3.0, step=20)  # writes back ctx
    runq.report({"d": 4.0})           # uses ctx (20)
    events = _read_events(tmp_path / "runq_metrics.jsonl")
    assert [(e["key"], e["step"]) for e in events] == [
        ("a", 10), ("b", 10), ("c", 20), ("d", 20),
    ]


# ---- history ------------------------------------------------------

def test_report_appends_to_history(clean_env, tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    runq.context()
    runq.report({"loss": 0.5}, step=1)
    runq.report({"loss": 0.4}, step=2)
    runq.report({"loss": 0.3}, step=3)
    h = _get_history_for_tests()
    assert [entry["step"] for entry in h] == [1, 2, 3]
    assert [entry["metrics"]["loss"] for entry in h] == [0.5, 0.4, 0.3]


def test_report_history_entry_is_copy_not_alias(clean_env, tmp_path, monkeypatch):
    """Hook mutation of the metrics dict must not poison history."""
    monkeypatch.chdir(tmp_path)
    runq.context()
    metrics = {"loss": 0.5}
    runq.report(metrics, step=1)
    metrics["loss"] = 9999.0  # post-hoc mutation of the caller's dict
    h = _get_history_for_tests()
    assert h[0]["metrics"]["loss"] == 0.5  # history kept the original


# ---- Decision return value (no hooks registered) -----------------

def test_report_no_hooks_returns_continue(clean_env, tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    runq.context()
    d = runq.report({"loss": 0.5}, step=1)
    assert d.should_stop is False
    assert d.reason is None


# ---- value coercion ----------------------------------------------

def test_report_coerces_int_value_to_float(clean_env, tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    runq.context()
    runq.report({"count": 42}, step=1)
    events = _read_events(tmp_path / "runq_metrics.jsonl")
    assert events[0]["value"] == 42.0
    assert isinstance(events[0]["value"], float)
