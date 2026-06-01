"""Tests for built-in early-stop policies + YAML auto-registration.

Three policy factories live in :mod:`runq._policies`:

- ``patience``   — best step + N idle → stop
- ``threshold``  — metric crosses bound → stop  (writeup-complete here)
- ``convergence`` — stddev over window < epsilon → stop

This file exercises each one end-to-end against the report flow, plus
the YAML auto-registration path and the @early_stop override behavior.
"""
import json
import logging

import pytest

import runq
from runq import _policies, _report


def _read_events(path):
    return [json.loads(line) for line in path.read_text().splitlines()]


# ---- patience -----------------------------------------------------

def test_patience_param_validation():
    """Construction-time errors fire on bad params (mode, patience, min_steps)."""
    with pytest.raises(ValueError, match="mode"):
        runq.patience(metric="loss", mode="lower")
    with pytest.raises(ValueError, match="patience"):
        runq.patience(metric="loss", patience=0)
    with pytest.raises(ValueError, match="min_steps"):
        runq.patience(metric="loss", min_steps=-1)


def test_patience_min_mode_stops_after_no_improvement(clean_env, tmp_path, monkeypatch):
    """val_loss strictly increasing for N steps after best → stop."""
    monkeypatch.chdir(tmp_path)
    runq.context()
    runq.early_stop(runq.patience(metric="val_loss", mode="min", patience=3))

    # Best at step 0. Then 3 strictly worse steps → stop.
    assert runq.report({"val_loss": 0.5}, step=0).should_stop is False
    assert runq.report({"val_loss": 0.6}, step=1).should_stop is False
    assert runq.report({"val_loss": 0.7}, step=2).should_stop is False
    d = runq.report({"val_loss": 0.8}, step=3)
    assert d.should_stop is True
    assert "patience" in d.reason


def test_patience_min_mode_resets_on_improvement(clean_env, tmp_path, monkeypatch):
    """An improvement resets the idle counter — patience must not fire."""
    monkeypatch.chdir(tmp_path)
    runq.context()
    runq.early_stop(runq.patience(metric="val_loss", mode="min", patience=2))

    runq.report({"val_loss": 0.5}, step=0)
    runq.report({"val_loss": 0.6}, step=1)  # 1 idle
    d = runq.report({"val_loss": 0.4}, step=2)  # improvement, reset
    assert d.should_stop is False
    d = runq.report({"val_loss": 0.5}, step=3)  # 1 idle from new best
    assert d.should_stop is False


def test_patience_max_mode(clean_env, tmp_path, monkeypatch):
    """mode=max: higher is better. Stop when accuracy stops climbing."""
    monkeypatch.chdir(tmp_path)
    runq.context()
    runq.early_stop(runq.patience(metric="acc", mode="max", patience=2))

    runq.report({"acc": 0.7}, step=0)
    runq.report({"acc": 0.6}, step=1)  # 1 idle
    d = runq.report({"acc": 0.5}, step=2)  # 2 idle → stop
    assert d.should_stop is True


def test_patience_min_steps_keeps_training_during_warmup(clean_env, tmp_path, monkeypatch):
    """min_steps overrides patience during warmup — even bad runs survive."""
    monkeypatch.chdir(tmp_path)
    runq.context()
    runq.early_stop(
        runq.patience(metric="val_loss", mode="min", patience=2, min_steps=10)
    )
    # Patience=2 would fire by step 3, but min_steps=10 holds it off.
    for s in range(5):
        d = runq.report({"val_loss": 0.5 + s * 0.1}, step=s)
        assert d.should_stop is False, f"min_steps=10 should still be in warmup at step {s}"


def test_patience_skips_entries_missing_metric(clean_env, tmp_path, monkeypatch):
    """Reports without the watched metric don't count toward patience."""
    monkeypatch.chdir(tmp_path)
    runq.context()
    runq.early_stop(runq.patience(metric="val_loss", mode="min", patience=2))

    # 1 with metric, then 2 without — patience must not fire.
    runq.report({"val_loss": 0.5}, step=0)
    runq.report({"train_loss": 0.4}, step=1)   # no val_loss
    runq.report({"train_loss": 0.3}, step=2)   # no val_loss
    d = runq.report({"train_loss": 0.2}, step=3)
    assert d.should_stop is False


# ---- threshold (fully implemented inline — sanity tests only) ----

def test_threshold_param_validation():
    with pytest.raises(ValueError, match="direction"):
        runq.threshold(metric="acc", direction="sideways", bound=0.9)


def test_threshold_above_fires(clean_env, tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    runq.context()
    runq.early_stop(runq.threshold(metric="acc", direction="above", bound=0.9))

    assert runq.report({"acc": 0.5}, step=0).should_stop is False
    assert runq.report({"acc": 0.89}, step=1).should_stop is False
    d = runq.report({"acc": 0.9}, step=2)
    assert d.should_stop is True
    assert "above" in d.reason


def test_threshold_below_fires(clean_env, tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    runq.context()
    runq.early_stop(runq.threshold(metric="val_loss", direction="below", bound=0.01))

    assert runq.report({"val_loss": 0.5}, step=0).should_stop is False
    d = runq.report({"val_loss": 0.005}, step=1)
    assert d.should_stop is True


def test_threshold_ignores_missing_metric(clean_env, tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    runq.context()
    runq.early_stop(runq.threshold(metric="acc", direction="above", bound=0.9))
    # Report different metric — threshold mustn't fire spuriously.
    d = runq.report({"train_loss": 0.9}, step=0)
    assert d.should_stop is False


# ---- convergence -------------------------------------------------

def test_convergence_param_validation():
    with pytest.raises(ValueError, match="window"):
        runq.convergence(metric="loss", window=1)
    with pytest.raises(ValueError, match="epsilon"):
        runq.convergence(metric="loss", window=5, epsilon=0)


def test_convergence_doesnt_fire_before_window_filled(clean_env, tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    runq.context()
    runq.early_stop(runq.convergence(metric="loss", window=5, epsilon=1e-3))

    # Only 3 entries — window of 5 not filled, must not fire.
    for s in range(3):
        d = runq.report({"loss": 1.0}, step=s)
        assert d.should_stop is False


def test_convergence_fires_when_stddev_below_epsilon(clean_env, tmp_path, monkeypatch):
    """Identical values → stddev=0 → fires when window fills."""
    monkeypatch.chdir(tmp_path)
    runq.context()
    runq.early_stop(runq.convergence(metric="loss", window=3, epsilon=1e-3))

    runq.report({"loss": 1.0}, step=0)
    runq.report({"loss": 1.0}, step=1)
    d = runq.report({"loss": 1.0}, step=2)
    assert d.should_stop is True
    assert "converged" in d.reason


def test_convergence_doesnt_fire_when_stddev_above_epsilon(clean_env, tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    runq.context()
    runq.early_stop(runq.convergence(metric="loss", window=3, epsilon=1e-3))

    runq.report({"loss": 1.0}, step=0)
    runq.report({"loss": 2.0}, step=1)
    d = runq.report({"loss": 3.0}, step=2)
    assert d.should_stop is False  # stddev=1.0 >> eps


# ---- YAML auto-registration --------------------------------------

def test_yaml_block_auto_registers_patience(clean_env, tmp_path, monkeypatch):
    """early_stopping block in params → built-in patience auto-registered."""
    params_file = tmp_path / "params.json"
    params_file.write_text(json.dumps({
        "early_stopping": {
            "policy": "patience",
            "metric": "val_loss",
            "mode": "min",
            "patience": 2,
        }
    }))
    monkeypatch.chdir(tmp_path)  # picks up params.json in manual mode
    runq.context()
    # The hook should be in the registry, untouched by user code.
    assert any(
        getattr(h, "_runq_yaml_policy", False) for h in _report._hooks
    ), "yaml policy should auto-register on context()"

    # And it should fire correctly.
    runq.report({"val_loss": 0.5}, step=0)
    runq.report({"val_loss": 0.6}, step=1)
    d = runq.report({"val_loss": 0.7}, step=2)
    assert d.should_stop is True


def test_yaml_default_policy_is_patience(clean_env, tmp_path, monkeypatch):
    """policy field omitted → defaults to patience."""
    params_file = tmp_path / "params.json"
    params_file.write_text(json.dumps({
        "early_stopping": {"metric": "val_loss", "patience": 2}
    }))
    monkeypatch.chdir(tmp_path)
    runq.context()
    # Just verify registration happened.
    assert any(
        getattr(h, "_runq_yaml_policy_name", "") == "patience"
        for h in _report._hooks
    )


def test_yaml_threshold_policy(clean_env, tmp_path, monkeypatch):
    params_file = tmp_path / "params.json"
    params_file.write_text(json.dumps({
        "early_stopping": {
            "policy": "threshold",
            "metric": "acc",
            "direction": "above",
            "bound": 0.95,
        }
    }))
    monkeypatch.chdir(tmp_path)
    runq.context()
    d = runq.report({"acc": 0.96}, step=0)
    assert d.should_stop is True


def test_yaml_no_block_no_auto_register(clean_env, tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    runq.context()
    yaml_hooks = [h for h in _report._hooks if getattr(h, "_runq_yaml_policy", False)]
    assert yaml_hooks == []


def test_yaml_unknown_policy_raises(clean_env, tmp_path, monkeypatch):
    params_file = tmp_path / "params.json"
    params_file.write_text(json.dumps({
        "early_stopping": {"policy": "magic", "metric": "x"}
    }))
    monkeypatch.chdir(tmp_path)
    with pytest.raises(ValueError, match="unknown early_stopping policy"):
        runq.context()


def test_yaml_invalid_block_kwarg_raises(clean_env, tmp_path, monkeypatch):
    """Typo in the block (e.g. 'mod' instead of 'mode') → clear ValueError."""
    params_file = tmp_path / "params.json"
    params_file.write_text(json.dumps({
        "early_stopping": {
            "policy": "patience",
            "metric": "val_loss",
            "mod": "min",     # typo
        }
    }))
    monkeypatch.chdir(tmp_path)
    with pytest.raises(ValueError, match="invalid early_stopping config"):
        runq.context()


# ---- decorator-overrides-YAML semantics --------------------------

def test_user_early_stop_overrides_yaml_with_warning(clean_env, tmp_path, monkeypatch, caplog):
    """@early_stop on user fn removes any YAML-registered policy + WARNs."""
    params_file = tmp_path / "params.json"
    params_file.write_text(json.dumps({
        "early_stopping": {
            "policy": "patience",
            "metric": "val_loss",
            "patience": 100,    # would never fire in this test
        }
    }))
    monkeypatch.chdir(tmp_path)
    runq.context()

    # Sanity: yaml hook is there.
    assert any(getattr(h, "_runq_yaml_policy", False) for h in _report._hooks)

    with caplog.at_level(logging.WARNING, logger="runq._report"):
        @runq.early_stop
        def stop_now(h, m):
            return "user override"

    # WARN emitted...
    assert any("overrides YAML" in r.message for r in caplog.records)
    # ...and yaml hook is gone.
    assert not any(getattr(h, "_runq_yaml_policy", False) for h in _report._hooks)

    # User's hook fires; yaml's would not have.
    d = runq.report({"val_loss": 0.5}, step=0)
    assert d.should_stop is True
    assert d.reason == "user override"


def test_second_user_early_stop_does_not_warn_again(clean_env, tmp_path, monkeypatch, caplog):
    """Eviction is one-shot — second @early_stop is silent."""
    params_file = tmp_path / "params.json"
    params_file.write_text(json.dumps({
        "early_stopping": {"policy": "patience", "metric": "loss", "patience": 5}
    }))
    monkeypatch.chdir(tmp_path)
    runq.context()

    @runq.early_stop
    def hook_a(h, m):
        return False

    # Clear the WARN emitted when hook_a evicted the YAML policy; we
    # only care about whether hook_b also warns.
    caplog.clear()

    with caplog.at_level(logging.WARNING, logger="runq._report"):
        @runq.early_stop
        def hook_b(h, m):
            return False

    assert not any("overrides YAML" in r.message for r in caplog.records)
