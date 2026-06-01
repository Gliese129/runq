"""Smoke tests for the demo scripts under ``sdk/python/examples/``.

Goal: catch the obvious breakage when SDK API names drift out from
under the demos. These tests are NOT a substitute for the live-
daemon e2e shell test (TODO Tier A item) — they only exercise SDK-
internal code paths.

Both demos pull in torch at import time, so the tests skip cleanly
when torch isn't installed in the test env.
"""
from __future__ import annotations

import importlib.util
import json
import shutil
import sys
from pathlib import Path
from typing import Any

import pytest

_EXAMPLES_DIR = Path(__file__).resolve().parent.parent / "examples"


def _load_demo(name: str) -> Any:
    """Load a demo script as a module without polluting sys.path.

    ``importlib.util.spec_from_file_location`` lets us import a file
    that lives outside the ``runq`` package without needing an
    ``examples/__init__.py`` (which would also drag the file into the
    wheel).
    """
    src = _EXAMPLES_DIR / f"{name}.py"
    assert src.exists(), f"missing demo file: {src}"
    spec = importlib.util.spec_from_file_location(f"_runq_demo_{name}", src)
    module = importlib.util.module_from_spec(spec)
    # Cache under a synthetic name so subsequent loads inside the same
    # test session don't reuse a stale state (e.g. lingering @early_stop
    # registrations from a prior demo invocation — though our
    # conftest's autouse fixture resets that anyway).
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


# ---- import-only smoke -----------------------------------------------

def test_freeze_demo_importable():
    pytest.importorskip("torch")
    m = _load_demo("freeze_demo")
    assert callable(m.train)
    assert callable(m.main)
    assert callable(m.save_checkpoint)  # confirms decorator stack worked


def test_no_daemon_demo_importable():
    pytest.importorskip("torch")
    m = _load_demo("no_daemon_demo")
    assert callable(m.train)


# ---- freeze_demo: happy-path end-to-end ------------------------------

def test_freeze_demo_train_completes(clean_env, tmp_path, monkeypatch):
    """Run ``train()`` in daemon mode against an unreachable socket.

    No disk pressure → no freeze-self HTTP call → demo completes
    normally. We use a fake socket path that the SDK never actually
    dials (safe_save's freeze path is gated on disk-short pre-check).

    Stub disk_usage to a fixed large free bytes so the pre-check
    always passes regardless of the host's real free space.
    """
    pytest.importorskip("torch")

    # Daemon-style env so ctx.mode == "daemon" and checkpoint_dir is set.
    monkeypatch.setenv("RUNQ_TASK_ID", "demo-1")
    monkeypatch.setenv("RUNQ_TASK_DIR", str(tmp_path))
    monkeypatch.setenv("RUNQ_METRICS_FILE", str(tmp_path / "metrics.jsonl"))
    monkeypatch.setenv("RUNQ_CHECKPOINT_DIR", str(tmp_path / "ckpts"))
    # Use a path the SDK won't try to dial because disk pre-check passes.
    monkeypatch.setenv("RUNQ_SOCKET_PATH", str(tmp_path / "fake.sock"))

    # Speed: shrink epochs via params.json that the demo's ctx.get
    # picks up.
    (tmp_path / "params.json").write_text(json.dumps({"epochs": 3, "lr": 0.1}))
    monkeypatch.setenv("RUNQ_PARAMS_FILE", str(tmp_path / "params.json"))

    # Force the pre-check to always succeed.
    def _fake_disk(_path):
        return shutil._ntuple_diskusage(1 << 50, 0, 1 << 50)

    monkeypatch.setattr(shutil, "disk_usage", _fake_disk)

    m = _load_demo("freeze_demo")
    rc = m.main()
    assert rc == 0

    # SDK should have written metrics for each epoch.
    events = [
        json.loads(line)
        for line in (tmp_path / "metrics.jsonl").read_text().splitlines()
        if line
    ]
    metric_events = [e for e in events if e["type"] == "metric"]
    assert len(metric_events) == 3
    # safe_save's manifest is populated under checkpoint_dir.
    manifest = tmp_path / "ckpts" / ".runq_manifest.json"
    assert manifest.exists()


# ---- no_daemon_demo: disk-full path ---------------------------------

def test_no_daemon_demo_hits_disk_full(clean_env, tmp_path, monkeypatch):
    """The demo deliberately uses size_hint=10**18 to trip the disk-
    short path; in ``no_daemon`` mode that raises ``RunqDiskFullError``
    (the demo catches it and returns 1).
    """
    pytest.importorskip("torch")

    monkeypatch.setenv("RUNQ_NO_DAEMON", "1")
    monkeypatch.setenv("RUNQ_TASK_ID", "demo-no-daemon")
    monkeypatch.setenv("RUNQ_TASK_DIR", str(tmp_path))
    monkeypatch.setenv("RUNQ_METRICS_FILE", str(tmp_path / "metrics.jsonl"))
    monkeypatch.setenv("RUNQ_CHECKPOINT_DIR", str(tmp_path / "ckpts"))

    m = _load_demo("no_daemon_demo")
    rc = m.train()
    assert rc == 1, f"expected disk-full exit (1), got {rc}"

    # At least one report fired before the disk-full crash.
    events_file = tmp_path / "metrics.jsonl"
    assert events_file.exists()
    events = [
        json.loads(line)
        for line in events_file.read_text().splitlines()
        if line
    ]
    assert any(e["type"] == "metric" for e in events)


def test_no_daemon_demo_refuses_wrong_mode(clean_env, tmp_path, monkeypatch):
    """Running the no_daemon demo in daemon mode returns exit 2 + warns."""
    pytest.importorskip("torch")

    # Daemon mode env — but no RUNQ_NO_DAEMON, so ctx.mode == "daemon".
    monkeypatch.setenv("RUNQ_TASK_ID", "demo-wrong-mode")
    monkeypatch.setenv("RUNQ_TASK_DIR", str(tmp_path))
    monkeypatch.setenv("RUNQ_METRICS_FILE", str(tmp_path / "metrics.jsonl"))
    monkeypatch.setenv("RUNQ_CHECKPOINT_DIR", str(tmp_path / "ckpts"))
    monkeypatch.setenv("RUNQ_SOCKET_PATH", str(tmp_path / "fake.sock"))

    m = _load_demo("no_daemon_demo")
    rc = m.train()
    assert rc == 2
