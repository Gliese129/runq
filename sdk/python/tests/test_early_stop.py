"""Tests for @runq.early_stop registration + hook orchestration.

The core algorithm under test is ``_run_early_stop_hooks`` in
``runq._report``. Mechanics:

- decorator registers fn into a module-level hook list
- ``report()`` runs hooks in order on each call
- first truthy return wins (short-circuit) — later hooks are skipped
- ``True`` → reason="user early_stop"; ``str`` → reason=that string
- falsy (None / False / "" / 0) → continue
- exceptions propagate (hooks shouldn't swallow user bugs)
"""
import pytest

import runq


# ---- registration --------------------------------------------------

def test_early_stop_registers_and_returns_fn(clean_env, tmp_path, monkeypatch):
    """The decorator returns the original function (still callable directly)."""
    monkeypatch.chdir(tmp_path)
    runq.context()

    @runq.early_stop
    def never_stop(history, metrics):
        return False

    # Decorator hands the function back so it can still be called directly.
    assert callable(never_stop)
    assert never_stop([], {"loss": 0.5}) is False


def test_early_stop_no_hooks_means_keep_going(clean_env, tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    runq.context()
    d = runq.report({"loss": 0.5}, step=1)
    assert d.should_stop is False


def test_early_stop_falsy_returns_keep_going(clean_env, tmp_path, monkeypatch):
    """Each of None / False / 0 / '' must be treated as 'continue'."""
    monkeypatch.chdir(tmp_path)
    runq.context()

    @runq.early_stop
    def returns_none(h, m):
        return None

    @runq.early_stop
    def returns_false(h, m):
        return False

    @runq.early_stop
    def returns_zero(h, m):
        return 0  # type: ignore[return-value]

    @runq.early_stop
    def returns_empty_string(h, m):
        return ""

    d = runq.report({"loss": 0.5})
    assert d.should_stop is False
    assert d.reason is None


# ---- truthy return paths ------------------------------------------

def test_early_stop_bool_true_uses_default_reason(clean_env, tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    runq.context()

    @runq.early_stop
    def stop_now(h, m):
        return True

    d = runq.report({"loss": 9.0})
    assert d.should_stop is True
    assert d.reason == "user early_stop"


def test_early_stop_string_return_used_as_reason(clean_env, tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    runq.context()

    @runq.early_stop
    def stop_with_reason(h, m):
        return "loss plateaued for 5 epochs"

    d = runq.report({"loss": 9.0})
    assert d.should_stop is True
    assert d.reason == "loss plateaued for 5 epochs"


# ---- short-circuit ordering ---------------------------------------

def test_early_stop_first_truthy_wins(clean_env, tmp_path, monkeypatch):
    """First registered truthy hook wins; later hooks must not fire."""
    monkeypatch.chdir(tmp_path)
    runq.context()
    call_log = []

    @runq.early_stop
    def hook_a(h, m):
        call_log.append("a")
        return "stop from A"

    @runq.early_stop
    def hook_b(h, m):
        call_log.append("b")
        return "stop from B"

    d = runq.report({"loss": 9.0})
    assert d.should_stop is True
    assert d.reason == "stop from A"  # registration order
    assert call_log == ["a"], "hook_b must not run once hook_a stopped"


def test_early_stop_skips_falsy_to_find_truthy(clean_env, tmp_path, monkeypatch):
    """Falsy hooks are passed over; the next truthy one decides."""
    monkeypatch.chdir(tmp_path)
    runq.context()
    call_log = []

    @runq.early_stop
    def passthru(h, m):
        call_log.append("passthru")
        return None

    @runq.early_stop
    def stopper(h, m):
        call_log.append("stopper")
        return True

    @runq.early_stop
    def never_reached(h, m):
        call_log.append("never_reached")
        return True

    d = runq.report({"loss": 9.0})
    assert d.should_stop is True
    assert call_log == ["passthru", "stopper"]


# ---- hook signature: history + current metrics --------------------

def test_early_stop_hook_sees_history(clean_env, tmp_path, monkeypatch):
    """Hook gets the running history (including the just-appended entry)."""
    monkeypatch.chdir(tmp_path)
    runq.context()
    seen = []

    @runq.early_stop
    def inspect(history, metrics):
        seen.append((len(history), metrics["loss"]))
        return False

    runq.report({"loss": 0.5}, step=1)
    runq.report({"loss": 0.4}, step=2)
    runq.report({"loss": 0.3}, step=3)
    # History length should reflect the *cumulative* count seen by each hook.
    assert seen == [(1, 0.5), (2, 0.4), (3, 0.3)]


def test_early_stop_hook_can_inspect_history_for_plateau(clean_env, tmp_path, monkeypatch):
    """Smoke test: a realistic plateau hook over 3+ entries fires correctly."""
    monkeypatch.chdir(tmp_path)
    runq.context()

    @runq.early_stop
    def plateau(history, metrics):
        if len(history) < 3:
            return False
        recent = [h["metrics"]["loss"] for h in history[-3:]]
        if max(recent) - min(recent) < 1e-6:
            return f"plateau over {len(recent)} epochs"
        return False

    assert runq.report({"loss": 0.5}, step=1).should_stop is False
    assert runq.report({"loss": 0.5}, step=2).should_stop is False
    d = runq.report({"loss": 0.5}, step=3)
    assert d.should_stop is True
    assert "plateau" in d.reason


# ---- duplicate registration --------------------------------------

def test_early_stop_idempotent_registration(clean_env, tmp_path, monkeypatch):
    """Registering the same fn twice should not double-fire."""
    monkeypatch.chdir(tmp_path)
    runq.context()
    n = [0]

    def hook(h, m):
        n[0] += 1
        return False

    runq.early_stop(hook)
    runq.early_stop(hook)  # duplicate registration

    runq.report({"loss": 0.5}, step=1)
    assert n[0] == 1, "duplicate registration must not multiply calls"


# ---- exception policy ---------------------------------------------

def test_early_stop_hook_exception_propagates(clean_env, tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    runq.context()

    @runq.early_stop
    def broken(h, m):
        raise ValueError("intentional bug in stop hook")

    with pytest.raises(ValueError, match="intentional bug"):
        runq.report({"loss": 0.5}, step=1)
