"""Tests for runq.log_metric — jsonl format, step handling, auto-step counter."""
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
    """step=0 is a legitimate value and must NOT be auto-replaced.

    Regression test for Codex review #6 — `step or auto` would treat 0 as
    missing. The correct check is `step is None`.
    """
    monkeypatch.chdir(tmp_path)
    runq.context()
    runq.log_metric("smoke", 1.0, step=0)
    events = _read_events(tmp_path / "runq_metrics.jsonl")
    assert events[0]["step"] == 0


def test_log_metric_auto_step_monotonic(clean_env, tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    runq.context()
    runq.log_metric("a", 1.0)  # auto 1
    runq.log_metric("b", 2.0)  # auto 2
    runq.log_metric("c", 3.0, step=99)  # explicit, doesn't bump counter
    runq.log_metric("d", 4.0)  # auto 3 (NOT 100)
    events = _read_events(tmp_path / "runq_metrics.jsonl")
    assert [e["step"] for e in events] == [1, 2, 99, 3]


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
