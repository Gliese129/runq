"""Tests for ctx.wandb_like_cfg / ctx.wandb_like_metric — F9.

Contract recap:
- Both properties return fresh dicts. No SDK import of wandb.
- wandb_like_cfg pulls project / name / group / config from ctx.
- wandb_like_metric returns the latest reported metrics dict, or {}.
- Returned dicts are copies — user mutation must not poison ctx /
  history.

These tests deliberately do not import wandb. If they ever start
needing wandb installed, the F9 design promise was broken.
"""
import sys

import pytest

import runq

# ---- pure-Python guarantee ----------------------------------------

def test_wandb_helpers_do_not_import_wandb(clean_env, tmp_path, monkeypatch):
    """Touching either property must not trigger ``import wandb``.

    We block wandb at the module-resolver level; any internal import
    inside the helpers would surface as a clear ImportError.
    """
    monkeypatch.setitem(sys.modules, "wandb", None)
    monkeypatch.chdir(tmp_path)
    ctx = runq.context()

    # Both properties must succeed even with wandb explicitly disabled.
    cfg = ctx.wandb_like_cfg
    metric = ctx.wandb_like_metric

    assert isinstance(cfg, dict)
    assert isinstance(metric, dict)


# ---- wandb_like_cfg shape ----------------------------------------

def test_wandb_like_cfg_manual_mode(clean_env, tmp_path, monkeypatch):
    """Manual mode: empty job_id → group=None, name=task_id, project='runq'."""
    monkeypatch.chdir(tmp_path)
    ctx = runq.context()
    cfg = ctx.wandb_like_cfg
    assert cfg["project"] == "runq"
    assert cfg["name"] == ctx.task_id
    assert cfg["group"] is None
    assert cfg["config"] == {}


def test_wandb_like_cfg_daemon_mode_shape(clean_env, tmp_path, monkeypatch):
    """Daemon mode: project / job_id / task_id present → all fields populated."""
    monkeypatch.setenv("RUNQ_TASK_ID", "t1")
    monkeypatch.setenv("RUNQ_JOB_ID", "j-abc")
    monkeypatch.setenv("RUNQ_PROJECT_NAME", "myproj")
    monkeypatch.setenv("RUNQ_TASK_DIR", str(tmp_path))
    monkeypatch.setenv("RUNQ_SOCKET_PATH", "/fake")
    ctx = runq.context()
    cfg = ctx.wandb_like_cfg
    assert cfg["project"] == "myproj"
    assert cfg["name"] == "j-abc/t1"
    assert cfg["group"] == "j-abc"


def test_wandb_like_cfg_includes_params(clean_env, tmp_path, monkeypatch):
    """ctx.params should appear under ``config`` key."""
    import json
    params_file = tmp_path / "params.json"
    params_file.write_text(json.dumps({"lr": 1e-4, "bs": 32}))
    monkeypatch.chdir(tmp_path)
    ctx = runq.context()
    cfg = ctx.wandb_like_cfg
    assert cfg["config"] == {"lr": 1e-4, "bs": 32}


def test_wandb_like_cfg_returns_copy_of_params(clean_env, tmp_path, monkeypatch):
    """Mutation of the returned config dict must not affect ctx.params."""
    import json
    params_file = tmp_path / "params.json"
    params_file.write_text(json.dumps({"lr": 1e-4}))
    monkeypatch.chdir(tmp_path)
    ctx = runq.context()
    cfg = ctx.wandb_like_cfg
    cfg["config"]["lr"] = 9999.0
    cfg["config"]["new_key"] = "hacked"
    # ctx.params unchanged.
    assert ctx.params == {"lr": 1e-4}


def test_wandb_like_cfg_project_default_when_unset(clean_env, tmp_path, monkeypatch):
    """Missing RUNQ_PROJECT_NAME → 'runq' fallback (sensible default)."""
    monkeypatch.setenv("RUNQ_TASK_ID", "t1")
    monkeypatch.setenv("RUNQ_TASK_DIR", str(tmp_path))
    monkeypatch.setenv("RUNQ_SOCKET_PATH", "/fake")
    ctx = runq.context()
    assert ctx.wandb_like_cfg["project"] == "runq"


# ---- wandb_like_metric shape -------------------------------------

def test_wandb_like_metric_empty_when_no_report(clean_env, tmp_path, monkeypatch):
    """Before any report(), should be {} (still safe to splat)."""
    monkeypatch.chdir(tmp_path)
    ctx = runq.context()
    assert ctx.wandb_like_metric == {}


def test_wandb_like_metric_returns_latest_report(clean_env, tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    ctx = runq.context()
    runq.report({"loss": 0.5, "acc": 0.9}, step=1)
    runq.report({"loss": 0.4, "acc": 0.95}, step=2)
    m = ctx.wandb_like_metric
    assert m == {"loss": 0.4, "acc": 0.95}


def test_wandb_like_metric_returns_copy(clean_env, tmp_path, monkeypatch):
    """Caller mutation of the returned dict must not poison history."""
    from runq._report import history_snapshot
    monkeypatch.chdir(tmp_path)
    ctx = runq.context()
    runq.report({"loss": 0.5}, step=1)
    m = ctx.wandb_like_metric
    m["loss"] = 9999.0
    m["new"] = "injected"
    # History stays clean.
    hist = history_snapshot()
    assert hist[0]["metrics"] == {"loss": 0.5}


def test_wandb_like_metric_splat_pattern(clean_env, tmp_path, monkeypatch):
    """Verify the canonical usage shape works: {**ctx.wandb_like_metric, ...}."""
    monkeypatch.chdir(tmp_path)
    ctx = runq.context()
    runq.report({"loss": 0.5}, step=1)
    merged = {**ctx.wandb_like_metric, "lr": 1e-4, "loss": 999.0}
    # User's extras + override should win on collision.
    assert merged == {"loss": 999.0, "lr": 1e-4}
