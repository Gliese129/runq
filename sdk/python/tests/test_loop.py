"""Tests for runq.range() — auto step + early-stop + preemption.

range() replaces the older loop() API. It yields integer steps, sets
ctx.current_step, checks the preemption flag, and reads _last_decision
from report() for early-stop.
"""
import json

import pytest

import runq
from runq._report import Decision


def _read_events(path):
    return [json.loads(line) for line in path.read_text().splitlines()]


# ---- basic iteration ----

def test_range_yields_all_when_no_stop(clean_env, tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    runq.context()
    seen = list(runq.range(5))
    assert seen == [0, 1, 2, 3, 4]


def test_range_start_stop_step(clean_env, tmp_path, monkeypatch):
    """range(start, stop, step) mirrors built-in range."""
    monkeypatch.chdir(tmp_path)
    runq.context()
    assert list(runq.range(2, 10, 3)) == [2, 5, 8]
    assert list(runq.range(5, 5)) == []


# ---- early stop integration ----

def test_range_breaks_on_should_stop(clean_env, tmp_path, monkeypatch):
    """A user hook returning True after iter 2 → range stops at iter 2."""
    monkeypatch.chdir(tmp_path)
    runq.context()

    @runq.early_stop
    def stop_at_2(history, current):
        return len(history) >= 3   # fires when 3rd report finishes

    seen = []
    for i in runq.range(10):
        seen.append(i)
        runq.report({"loss": 1.0 / (i + 1)}, step=i)

    # iter 0 → report → hook returns False (history=1)
    # iter 1 → report → False (history=2)
    # iter 2 → report → True (history=3) → ctx._last_decision.should_stop=True
    # Next range check: break before iter 3.
    assert seen == [0, 1, 2]


def test_range_writes_loop_break_event(clean_env, tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    runq.context()

    @runq.early_stop
    def stop_immediately(history, current):
        return "test stop"

    for i in runq.range(10):
        runq.report({"loss": 0.5}, step=i)
        # Break should fire after this iter.

    events = _read_events(tmp_path / "runq_metrics.jsonl")
    breaks = [e for e in events if e["type"] == "loop_break"]
    assert len(breaks) == 1
    assert breaks[0]["name"] == "range"
    assert breaks[0]["reason"] == "test stop"


def test_range_runs_full_when_no_report(clean_env, tmp_path, monkeypatch):
    """Without any report() calls, decision is None → no break."""
    monkeypatch.chdir(tmp_path)
    runq.context()
    seen = list(runq.range(3))
    assert seen == [0, 1, 2]


def test_range_name_default_is_range(clean_env, tmp_path, monkeypatch):
    """loop_break event uses 'range' as default name."""
    monkeypatch.chdir(tmp_path)
    runq.context()

    @runq.early_stop
    def stop(history, current):
        return True

    for i in runq.range(3):
        runq.report({"x": 1.0}, step=i)

    events = _read_events(tmp_path / "runq_metrics.jsonl")
    breaks = [e for e in events if e["type"] == "loop_break"]
    assert breaks[0]["name"] == "range"


# ---- step propagation ----

def test_range_sets_current_step_per_iteration(clean_env, tmp_path, monkeypatch):
    """ctx.current_step reflects the active iter index inside the range body."""
    monkeypatch.chdir(tmp_path)
    ctx = runq.context()
    seen = []
    for _ in runq.range(4):
        seen.append(ctx.current_step)
    assert seen == [0, 1, 2, 3]


def test_range_step_default_propagates_to_report(clean_env, tmp_path, monkeypatch):
    """report() without explicit step picks up the range's current_step."""
    monkeypatch.chdir(tmp_path)
    runq.context()
    for _ in runq.range(3):
        runq.report({"x": 1.0})   # no step= — should fall back to ctx
    events = _read_events(tmp_path / "runq_metrics.jsonl")
    assert [e["step"] for e in events] == [0, 1, 2]


def test_range_explicit_step_overrides_index(clean_env, tmp_path, monkeypatch):
    """Per-call explicit step at the report site wins; writes back to ctx."""
    monkeypatch.chdir(tmp_path)
    ctx = runq.context()
    for i in runq.range(3):
        runq.report({"x": 1.0}, step=i * 100)
        assert ctx.current_step == i * 100   # writeback overrode the range's i


def test_range_current_step_persists_after_normal_exit(clean_env, tmp_path, monkeypatch):
    """After the range runs to completion, ctx.current_step holds the last i."""
    monkeypatch.chdir(tmp_path)
    ctx = runq.context()
    for _ in runq.range(5):
        pass
    assert ctx.current_step == 4


# ---- preemption ----

def test_range_stops_on_preempted_flag(clean_env, tmp_path, monkeypatch):
    """Setting _preempted flag → range exits cleanly."""
    monkeypatch.chdir(tmp_path)
    runq.context()

    from runq import _range
    seen = []
    for i in runq.range(10):
        seen.append(i)
        if i == 2:
            _range._preempted = True

    # iter 0, 1, 2 run; flag set at 2; next check at 3 exits
    assert seen == [0, 1, 2]

    events = _read_events(tmp_path / "runq_metrics.jsonl")
    preempts = [e for e in events if e["type"] == "preempted"]
    assert len(preempts) == 1
    assert preempts[0]["step"] == 3
