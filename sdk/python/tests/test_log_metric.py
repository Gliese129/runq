"""Tests for runq.log_metric — jsonl format, step handling.

Step 6 changed the step semantics: the hidden auto-step counter is
gone. ``step=None`` falls back to ``ctx.current_step`` (which may
itself be None). Explicit ``step=N`` writes back to ctx so log_metric
and report stay in sync.
"""
import json

import runq


def _read_events(path):
    """Read all events out of a jsonl file."""
    return [json.loads(line) for line in path.read_text().splitlines()]


def test_log_metric_basic_format(clean_env, tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    runq.context()
    runq.log_metric("loss", 0.42, step=5)
    events = _read_events(tmp_path / "runq_metrics.jsonl")
    assert len(events) == 1
    e = events[0]
    assert e["type"] == "metric"
    assert e["key"] == "loss"
    assert e["value"] == 0.42
    assert e["step"] == 5
    assert isinstance(e["ts"], int)
    # SDK does NOT write task_id / job_id (daemon fills on reap).
    assert "task_id" not in e
    assert "job_id" not in e


def test_log_metric_step_zero_preserved(clean_env, tmp_path, monkeypatch):
    """step=0 is a legitimate value and must NOT be replaced with ctx fallback.

    Regression test for Codex review #6 — `step or ctx_step` would treat
    0 as missing. The correct check is `step is None`.
    """
    monkeypatch.chdir(tmp_path)
    runq.context()
    runq.log_metric("smoke", 1.0, step=0)
    events = _read_events(tmp_path / "runq_metrics.jsonl")
    assert events[0]["step"] == 0


def test_log_metric_step_none_uses_ctx_fallback(clean_env, tmp_path, monkeypatch):
    """step=None reads ctx.current_step (set by a prior explicit call)."""
    monkeypatch.chdir(tmp_path)
    ctx = runq.context()
    runq.log_metric("a", 1.0, step=7)   # sets ctx.current_step=7
    runq.log_metric("b", 2.0)           # no step → uses ctx (7)
    runq.log_metric("c", 3.0)           # still 7
    events = _read_events(tmp_path / "runq_metrics.jsonl")
    assert [e["step"] for e in events] == [7, 7, 7]
    assert ctx.current_step == 7


def test_log_metric_step_none_with_unset_ctx_writes_null(clean_env, tmp_path, monkeypatch):
    """When nothing ever set the step, jsonl gets step=null (not auto-1)."""
    monkeypatch.chdir(tmp_path)
    runq.context()
    runq.log_metric("a", 1.0)
    runq.log_metric("b", 2.0)
    events = _read_events(tmp_path / "runq_metrics.jsonl")
    assert events[0]["step"] is None
    assert events[1]["step"] is None


def test_log_metric_explicit_step_writes_back_to_ctx(clean_env, tmp_path, monkeypatch):
    """Each explicit step= updates ctx so subsequent calls see the new value."""
    monkeypatch.chdir(tmp_path)
    ctx = runq.context()
    runq.log_metric("a", 1.0, step=3)
    assert ctx.current_step == 3
    runq.log_metric("b", 2.0, step=10)
    assert ctx.current_step == 10
    runq.log_metric("c", 3.0)
    events = _read_events(tmp_path / "runq_metrics.jsonl")
    assert [e["step"] for e in events] == [3, 10, 10]


def test_log_metric_appends_no_overwrite(clean_env, tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    runq.context()
    for i in range(5):
        runq.log_metric("x", float(i), step=i)
    events = _read_events(tmp_path / "runq_metrics.jsonl")
    assert len(events) == 5
    assert [e["value"] for e in events] == [0.0, 1.0, 2.0, 3.0, 4.0]


def test_log_metric_writes_to_env_path(clean_env, tmp_path, monkeypatch):
    """daemon mode: jsonl path comes from RUNQ_METRICS_FILE, not cwd."""
    custom = tmp_path / "subdir" / "custom.jsonl"
    monkeypatch.setenv("RUNQ_TASK_ID", "t1")
    monkeypatch.setenv("RUNQ_TASK_DIR", str(tmp_path))
    monkeypatch.setenv("RUNQ_METRICS_FILE", str(custom))
    monkeypatch.setenv("RUNQ_SOCKET_PATH", "/fake")
    runq.context()
    runq.log_metric("loss", 0.5, step=1)
    assert custom.exists()
    events = _read_events(custom)
    assert events[0]["key"] == "loss"


def test_log_metric_float_coerces_int(clean_env, tmp_path, monkeypatch):
    """Pass an int, SDK stores as float — keeps the wire format consistent."""
    monkeypatch.chdir(tmp_path)
    runq.context()
    runq.log_metric("count", 42, step=1)
    events = _read_events(tmp_path / "runq_metrics.jsonl")
    assert events[0]["value"] == 42.0
    assert isinstance(events[0]["value"], float)
