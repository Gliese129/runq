"""Tests for runq.context() — mode detection, env parsing, params loading."""
import json
import os

import pytest

import runq
from runq._context import _detect_mode

# ---- mode detection ----

def test_manual_mode_no_env(clean_env, tmp_path, monkeypatch):
    """No RUNQ_TASK_ID → manual mode, jsonl in cwd, local UUID task_id."""
    monkeypatch.chdir(tmp_path)
    ctx = runq.context()
    assert ctx.mode == "manual"
    assert ctx.task_id.startswith("manual-")
    assert ctx.metrics_file == tmp_path / "runq_metrics.jsonl"
    assert ctx.checkpoint_dir is None  # manual has no ckpt dir
    assert ctx.socket_path is None


def test_no_daemon_mode_explicit(clean_env, tmp_path, monkeypatch):
    """RUNQ_NO_DAEMON=1 + RUNQ_TASK_ID → no_daemon even if socket also set."""
    monkeypatch.setenv("RUNQ_TASK_ID", "t1")
    monkeypatch.setenv("RUNQ_TASK_DIR", str(tmp_path))
    monkeypatch.setenv("RUNQ_METRICS_FILE", str(tmp_path / "metrics.jsonl"))
    monkeypatch.setenv("RUNQ_NO_DAEMON", "1")
    monkeypatch.setenv("RUNQ_SOCKET_PATH", "/fake/should/be/ignored")
    ctx = runq.context()
    assert ctx.mode == "no_daemon"
    assert ctx.task_id == "t1"


def test_no_daemon_mode_when_socket_missing(clean_env, tmp_path, monkeypatch):
    """RUNQ_TASK_ID set but no socket path → no_daemon (HPC path)."""
    monkeypatch.setenv("RUNQ_TASK_ID", "t1")
    monkeypatch.setenv("RUNQ_TASK_DIR", str(tmp_path))
    monkeypatch.setenv("RUNQ_METRICS_FILE", str(tmp_path / "metrics.jsonl"))
    ctx = runq.context()
    assert ctx.mode == "no_daemon"


def test_daemon_mode(clean_env, tmp_path, monkeypatch):
    """RUNQ_TASK_ID + RUNQ_SOCKET_PATH + no RUNQ_NO_DAEMON → daemon."""
    monkeypatch.setenv("RUNQ_TASK_ID", "t1")
    monkeypatch.setenv("RUNQ_TASK_DIR", str(tmp_path))
    monkeypatch.setenv("RUNQ_METRICS_FILE", str(tmp_path / "metrics.jsonl"))
    monkeypatch.setenv("RUNQ_SOCKET_PATH", "/tmp/runq.sock")
    ctx = runq.context()
    assert ctx.mode == "daemon"
    assert ctx.socket_path == "/tmp/runq.sock"


def test_runq_no_daemon_truthy_variations(clean_env, monkeypatch):
    """Verify the loose truthy parse on RUNQ_NO_DAEMON."""
    monkeypatch.setenv("RUNQ_TASK_ID", "t1")
    monkeypatch.setenv("RUNQ_SOCKET_PATH", "/fake")

    for truthy in ("1", "true", "TRUE", "yes", "on"):
        monkeypatch.setenv("RUNQ_NO_DAEMON", truthy)
        assert _detect_mode() == "no_daemon", f"expected no_daemon for {truthy!r}"

    for falsy in ("0", "", "false", "no", "off"):
        monkeypatch.setenv("RUNQ_NO_DAEMON", falsy)
        assert _detect_mode() == "daemon", f"expected daemon for {falsy!r}"


# ---- params loading ----

def test_params_loaded_from_env_path(clean_env, tmp_path, monkeypatch):
    params_file = tmp_path / "params.json"
    params_file.write_text(json.dumps({"lr": 1e-4, "bs": 32}))
    monkeypatch.setenv("RUNQ_TASK_ID", "t1")
    monkeypatch.setenv("RUNQ_TASK_DIR", str(tmp_path))
    monkeypatch.setenv("RUNQ_METRICS_FILE", str(tmp_path / "metrics.jsonl"))
    monkeypatch.setenv("RUNQ_PARAMS_FILE", str(params_file))

    ctx = runq.context()
    assert ctx.params == {"lr": 1e-4, "bs": 32}
    assert ctx.get("lr") == 1e-4
    assert ctx.get("missing", 99) == 99


def test_params_manual_mode_picks_up_local(clean_env, tmp_path, monkeypatch):
    """In manual mode, a local params.json in cwd is loaded automatically."""
    monkeypatch.chdir(tmp_path)
    (tmp_path / "params.json").write_text(json.dumps({"x": 1}))
    ctx = runq.context()
    assert ctx.mode == "manual"
    assert ctx.params == {"x": 1}


def test_params_missing_is_empty_dict(clean_env, tmp_path, monkeypatch):
    """No params file in any mode → empty dict, not error."""
    monkeypatch.chdir(tmp_path)
    ctx = runq.context()
    assert ctx.params == {}


def test_params_broken_json_raises(clean_env, tmp_path, monkeypatch):
    """Malformed params.json should surface, not silently produce {}."""
    monkeypatch.chdir(tmp_path)
    (tmp_path / "params.json").write_text("{not valid json")
    with pytest.raises(RuntimeError, match="failed to load params"):
        runq.context()


def test_params_non_dict_raises(clean_env, tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    (tmp_path / "params.json").write_text(json.dumps([1, 2, 3]))
    with pytest.raises(RuntimeError, match="must contain a JSON object"):
        runq.context()


# ---- singleton + get_ctx ----

def test_context_is_idempotent(clean_env, tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    a = runq.context()
    b = runq.context()
    assert a is b


def test_get_ctx_before_init_raises(clean_env):
    with pytest.raises(RuntimeError, match="context\\(\\) must be called"):
        runq.get_ctx()


def test_get_ctx_after_init(clean_env, tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    ctx = runq.context()
    assert runq.get_ctx() is ctx


# ---- safety factor / extra GB env parsing ----

def test_safety_envs_parsed(clean_env, tmp_path, monkeypatch):
    monkeypatch.setenv("RUNQ_TASK_ID", "t1")
    monkeypatch.setenv("RUNQ_TASK_DIR", str(tmp_path))
    monkeypatch.setenv("RUNQ_METRICS_FILE", str(tmp_path / "metrics.jsonl"))
    monkeypatch.setenv("RUNQ_SAFETY_FACTOR_PERCENT", "150")
    monkeypatch.setenv("RUNQ_SAFETY_EXTRA_GB", "5")
    ctx = runq.context()
    assert ctx.safety_factor_percent == 150
    assert ctx.safety_extra_gb == 5


def test_safety_envs_default_when_unset(clean_env, tmp_path, monkeypatch):
    """daemon doesn't set these in manual mode → SDK defaults apply."""
    monkeypatch.chdir(tmp_path)
    ctx = runq.context()
    assert ctx.safety_factor_percent == 110
    assert ctx.safety_extra_gb == 0


def test_safety_envs_garbage_falls_back_to_default(clean_env, tmp_path, monkeypatch):
    monkeypatch.setenv("RUNQ_TASK_ID", "t1")
    monkeypatch.setenv("RUNQ_TASK_DIR", str(tmp_path))
    monkeypatch.setenv("RUNQ_METRICS_FILE", str(tmp_path / "metrics.jsonl"))
    monkeypatch.setenv("RUNQ_SAFETY_FACTOR_PERCENT", "not-a-number")
    ctx = runq.context()
    assert ctx.safety_factor_percent == 110  # fell back to default


# ---- dir creation ----

def test_checkpoint_dir_auto_created(clean_env, tmp_path, monkeypatch):
    """context() pre-creates checkpoint_dir to avoid first-save failure."""
    ckpt = tmp_path / "deep" / "nested" / "ckpts"
    monkeypatch.setenv("RUNQ_TASK_ID", "t1")
    monkeypatch.setenv("RUNQ_TASK_DIR", str(tmp_path))
    monkeypatch.setenv("RUNQ_METRICS_FILE", str(tmp_path / "metrics.jsonl"))
    monkeypatch.setenv("RUNQ_CHECKPOINT_DIR", str(ckpt))
    runq.context()
    assert ckpt.is_dir()
