"""Tests for runq.loop — yield + auto early-stop break.

Loop reads ctx._last_decision, which report() writes. The
integration test below uses a registered @early_stop hook; the unit
tests set ctx._last_decision directly to keep the test minimal.
"""
import json

import pytest

import runq
from runq._report import Decision


def _read_events(path):
    return [json.loads(line) for line in path.read_text().splitlines()]


# ---- basic iteration ----

def test_loop_yields_all_when_no_stop(clean_env, tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    runq.context()
    seen = list(runq.loop(range(5)))
    assert seen == [0, 1, 2, 3, 4]


def test_loop_yields_arbitrary_iterables(clean_env, tmp_path, monkeypatch):
    """Generator / list / tuple all work."""
    monkeypatch.chdir(tmp_path)
    runq.context()
    assert list(runq.loop([10, 20, 30])) == [10, 20, 30]
    assert list(runq.loop(i * i for i in range(4))) == [0, 1, 4, 9]


# ---- early stop integration ----

def test_loop_breaks_on_should_stop(clean_env, tmp_path, monkeypatch):
    """A user hook returning True after iter 2 → loop stops at iter 2."""
    monkeypatch.chdir(tmp_path)
    runq.context()

    @runq.early_stop
    def stop_at_2(history, current):
        return len(history) >= 3   # fires when 3rd report finishes

    seen = []
    for i in runq.loop(range(10)):
        seen.append(i)
        runq.report({"loss": 1.0 / (i + 1)}, step=i)

    # iter 0 → report → hook returns False (history=1)
    # iter 1 → report → False (history=2)
    # iter 2 → report → True (history=3) → ctx._last_decision.should_stop=True
    # Next loop check: break before iter 3.
    assert seen == [0, 1, 2]


def test_loop_writes_loop_break_event(clean_env, tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    runq.context()

    @runq.early_stop
    def stop_immediately(history, current):
        return "test stop"

    for i in runq.loop(range(10), name="training"):
        runq.report({"loss": 0.5}, step=i)
        # Break should fire after this iter.

    events = _read_events(tmp_path / "runq_metrics.jsonl")
    breaks = [e for e in events if e["type"] == "loop_break"]
    assert len(breaks) == 1
    assert breaks[0]["name"] == "training"
    assert breaks[0]["reason"] == "test stop"


def test_loop_runs_full_iterable_with_no_report(clean_env, tmp_path, monkeypatch):
    """Without any report() calls, decision is None → no break."""
    monkeypatch.chdir(tmp_path)
    runq.context()
    seen = list(runq.loop(range(3)))
    assert seen == [0, 1, 2]


def test_loop_clears_stale_decision_from_prior_run(clean_env, tmp_path, monkeypatch):
    """A leftover Decision in ctx must NOT immediately break a fresh loop.

    Notebook scenario: first loop set _last_decision; user re-runs cell.
    """
    monkeypatch.chdir(tmp_path)
    ctx = runq.context()
    # Simulate prior decision.
    ctx._last_decision = Decision(should_stop=True, reason="old run")

    # Fresh loop should not see the stale decision on iter 0.
    seen = list(runq.loop(range(3)))
    assert seen == [0, 1, 2]


def test_loop_name_default_is_loop(clean_env, tmp_path, monkeypatch):
    """When name= not passed, loop_break event uses 'loop'."""
    monkeypatch.chdir(tmp_path)
    runq.context()

    @runq.early_stop
    def stop(history, current):
        return True

    for i in runq.loop(range(3)):
        runq.report({"x": 1.0}, step=i)

    events = _read_events(tmp_path / "runq_metrics.jsonl")
    breaks = [e for e in events if e["type"] == "loop_break"]
    assert breaks[0]["name"] == "loop"


# ---- step propagation (F5.5 — loop is the default step source) ----

def test_loop_sets_current_step_per_iteration(clean_env, tmp_path, monkeypatch):
    """ctx.current_step reflects the active iter index inside the loop body."""
    monkeypatch.chdir(tmp_path)
    ctx = runq.context()
    seen = []
    for _ in runq.loop(range(4)):
        seen.append(ctx.current_step)
    assert seen == [0, 1, 2, 3]


def test_loop_step_default_propagates_to_report(clean_env, tmp_path, monkeypatch):
    """report() without explicit step picks up the loop's current_step."""
    monkeypatch.chdir(tmp_path)
    runq.context()
    for _ in runq.loop(range(3)):
        runq.report({"x": 1.0})   # no step= — should fall back to ctx
    events = _read_events(tmp_path / "runq_metrics.jsonl")
    assert [e["step"] for e in events] == [0, 1, 2]


def test_loop_explicit_step_overrides_loop_index(clean_env, tmp_path, monkeypatch):
    """Per-call explicit step at the report site wins; writes back to ctx."""
    monkeypatch.chdir(tmp_path)
    ctx = runq.context()
    for i in runq.loop(range(3)):
        runq.report({"x": 1.0}, step=i * 100)
        assert ctx.current_step == i * 100   # writeback overrode the loop's i


def test_loop_current_step_persists_after_normal_exit(clean_env, tmp_path, monkeypatch):
    """After the loop runs to completion, ctx.current_step holds the last i."""
    monkeypatch.chdir(tmp_path)
    ctx = runq.context()
    for _ in runq.loop(range(5)):
        pass
    assert ctx.current_step == 4


# ---- tqdm graceful fallback ----

def test_loop_works_without_tqdm(clean_env, tmp_path, monkeypatch):
    """If tqdm is not importable, loop still works (silent fallback)."""
    import sys
    monkeypatch.setitem(sys.modules, "tqdm", None)
    monkeypatch.chdir(tmp_path)
    runq.context()
    seen = list(runq.loop(range(3), name="t"))
    assert seen == [0, 1, 2]
