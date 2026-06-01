"""Tests for runq.sync_now — subprocess wrapper around `runq sync` CLI.

The function is best-effort and the explicit-exception catch policy is
intentional. Unexpected exception types should surface to the user,
not get silently swallowed; future Python / subprocess changes that
add new exception classes should fail loudly so we can update the
catch list deliberately.

Tests exercise:

- happy path: subprocess exit 0 → True
- non-zero exit → False
- FileNotFoundError (binary missing) → False
- PermissionError (not executable) → False
- TimeoutExpired (hung) → False
- OSError (kernel-level) → False
- manual mode (no task_dir) → False without invoking subprocess
- argv shape: ``["runq", "sync", "--task-dir", <ctx.task_dir>]``
- timeout kwarg propagates to subprocess.run

We mock ``subprocess.run`` via ``monkeypatch.setattr`` so the tests
don't need an actual ``runq`` binary on PATH. The wrapper passes argv
as the ``args=`` kwarg (rather than positionally), which the helpers
below normalize away from the caller.
"""
import subprocess
from pathlib import Path

import pytest

import runq
from runq import _sync

# ---- helpers ----

class _FakeCompleted:
    """Stand-in for subprocess.CompletedProcess; only `returncode` is read."""

    def __init__(self, returncode: int = 0, stderr: str = ""):
        self.returncode = returncode
        self.stderr = stderr
        self.stdout = ""


def _install_fake_run(monkeypatch, behavior):
    """Patch subprocess.run with ``behavior``.

    ``behavior`` is a callable ``(*args, **kwargs) -> result`` or one
    that raises. Returns the list of recorded calls so the test can
    assert what was invoked. The recorded entries carry both
    positional ``args`` and keyword ``kwargs`` exactly as the wrapper
    invoked them.
    """
    calls = []

    def fake_run(*args, **kwargs):
        calls.append({"args": args, "kwargs": kwargs})
        return behavior(*args, **kwargs)

    monkeypatch.setattr(_sync.subprocess, "run", fake_run)
    return calls


def _extract_argv(call):
    """Pull the argv list whether sync_now passed it positionally or as kwarg.

    The current implementation passes ``args=argv`` (kwarg form), but
    a future refactor that switches to positional is fine — the tests
    shouldn't lock the calling convention.
    """
    if "args" in call["kwargs"]:
        return call["kwargs"]["args"]
    return call["args"][0]


@pytest.fixture
def task_ctx(clean_env, tmp_path, monkeypatch):
    """A ctx with a non-None task_dir (so sync_now actually invokes subprocess)."""
    monkeypatch.setenv("RUNQ_TASK_ID", "t1")
    monkeypatch.setenv("RUNQ_TASK_DIR", str(tmp_path))
    monkeypatch.setenv("RUNQ_NO_DAEMON", "1")   # don't try to touch a socket
    return runq.context()


# ---- happy path ------------------------------------------------------

def test_sync_now_returns_true_on_success(task_ctx, monkeypatch):
    _install_fake_run(monkeypatch, lambda *a, **k: _FakeCompleted(returncode=0))
    assert runq.sync_now() is True


def test_sync_now_returns_false_on_nonzero_exit(task_ctx, monkeypatch):
    _install_fake_run(
        monkeypatch,
        lambda *a, **k: _FakeCompleted(returncode=2, stderr="sqlite locked"),
    )
    assert runq.sync_now() is False


# ---- failure modes ---------------------------------------------------

def test_sync_now_swallows_filenotfound(task_ctx, monkeypatch):
    """`runq` binary missing on PATH → False, no raise."""
    def boom(*args, **kwargs):
        raise FileNotFoundError(2, "No such file or directory: 'runq'")

    _install_fake_run(monkeypatch, boom)
    assert runq.sync_now() is False


def test_sync_now_swallows_permissionerror(task_ctx, monkeypatch):
    """Binary present but not executable."""
    def boom(*args, **kwargs):
        raise PermissionError(13, "Permission denied")

    _install_fake_run(monkeypatch, boom)
    assert runq.sync_now() is False


def test_sync_now_swallows_timeout(task_ctx, monkeypatch):
    """Subprocess hangs past timeout_s → False, no propagate."""
    def boom(*args, **kwargs):
        argv = kwargs.get("args") if "args" in kwargs else args[0]
        raise subprocess.TimeoutExpired(cmd=argv, timeout=kwargs.get("timeout"))

    _install_fake_run(monkeypatch, boom)
    assert runq.sync_now(timeout_s=0.001) is False


def test_sync_now_swallows_oserror(task_ctx, monkeypatch):
    """Catch-all for kernel-level failures (e.g. ENOMEM forking)."""
    def boom(*args, **kwargs):
        raise OSError(12, "Cannot allocate memory")

    _install_fake_run(monkeypatch, boom)
    assert runq.sync_now() is False


def test_sync_now_unknown_exception_propagates(task_ctx, monkeypatch):
    """Exception types NOT in the explicit catch list propagate.

    Design choice: a surprise exception (e.g. a new ``subprocess.*``
    error in a future Python release) is a signal to update the
    handler list deliberately, not something to silently swallow.
    If the implementation ever adds a broad ``except Exception``
    fallback, this test breaks loudly so we re-think the decision.
    """
    class WeirdError(Exception):
        pass

    def boom(*args, **kwargs):
        raise WeirdError("unexpected")

    _install_fake_run(monkeypatch, boom)
    with pytest.raises(WeirdError):
        runq.sync_now()


# ---- manual mode -----------------------------------------------------

def test_sync_now_manual_mode_returns_false_without_subprocess(clean_env, tmp_path, monkeypatch):
    """ctx.task_dir is None in manual mode → no subprocess invocation."""
    monkeypatch.chdir(tmp_path)
    runq.context()   # manual mode, task_dir=None
    calls = _install_fake_run(monkeypatch, lambda *a, **k: _FakeCompleted(0))

    assert runq.sync_now() is False
    assert calls == [], "manual mode should not invoke subprocess.run"


# ---- argv + kwargs shape --------------------------------------------

def test_sync_now_passes_task_dir_argv(task_ctx, monkeypatch):
    """argv must include the canonical 'runq sync --task-dir <task_dir>' shape."""
    calls = _install_fake_run(monkeypatch, lambda *a, **k: _FakeCompleted(0))
    runq.sync_now()
    assert len(calls) == 1
    argv = _extract_argv(calls[0])
    assert argv[0] == "runq"
    assert argv[1] == "sync"
    assert "--task-dir" in argv
    # task_dir position right after the flag.
    assert argv[argv.index("--task-dir") + 1] == str(task_ctx.task_dir)


def test_sync_now_passes_timeout_to_subprocess(task_ctx, monkeypatch):
    """The timeout_s kwarg propagates to subprocess.run."""
    calls = _install_fake_run(monkeypatch, lambda *a, **k: _FakeCompleted(0))
    runq.sync_now(timeout_s=2.5)
    assert calls[0]["kwargs"].get("timeout") == 2.5


def test_sync_now_default_timeout_matches_module_constant(task_ctx, monkeypatch):
    """When called without timeout_s, _DEFAULT_TIMEOUT_S is used."""
    calls = _install_fake_run(monkeypatch, lambda *a, **k: _FakeCompleted(0))
    runq.sync_now()
    assert calls[0]["kwargs"].get("timeout") == _sync._DEFAULT_TIMEOUT_S


def test_sync_now_does_not_print_to_stdout(task_ctx, monkeypatch, capsys):
    """Sync failures must be silent on stdout/stderr (best-effort policy)."""
    _install_fake_run(
        monkeypatch,
        lambda *a, **k: _FakeCompleted(returncode=2, stderr="some error"),
    )
    runq.sync_now()
    out = capsys.readouterr()
    assert out.out == ""
    # stderr coming from the CLI is captured by subprocess.run (capture_output=True)
    # — sync_now itself should not write to its own stderr either.
    assert out.err == ""


# ---- argv helper -----------------------------------------------------

def test_build_argv_shape():
    """Direct unit test on the argv helper — independent of subprocess mocks."""
    argv = _sync._build_argv("/tmp/runq/task-abc")
    assert argv == ["runq", "sync", "--task-dir", "/tmp/runq/task-abc"]
