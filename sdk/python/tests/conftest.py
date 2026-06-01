"""Shared pytest fixtures for the runq SDK test suite.

Most tests need a fresh singleton state — the SDK keeps a module-level
Context which would leak between tests. The autouse `_reset_sdk`
fixture below wipes both the context singleton and the auto-step
counter before every test.
"""
import pytest

from runq import _context, _events, _prefix, _range, _report


@pytest.fixture(autouse=True)
def _reset_sdk():
    """Reset all SDK module-level state before each test."""
    _context._reset_for_tests()
    _events._reset_for_tests()
    _report._reset_for_tests()
    _range._reset_for_tests()
    # Reset the log_group prefix stack — otherwise a test that
    # crashes inside `with log_group():` would leak its prefix into
    # the next test's metric keys.
    _prefix._prefix_stack.set(())
    yield
    _context._reset_for_tests()
    _events._reset_for_tests()
    _report._reset_for_tests()
    _range._reset_for_tests()
    _prefix._prefix_stack.set(())


@pytest.fixture
def clean_env(monkeypatch):
    """Clear every RUNQ_* env var so tests start from a known blank slate."""
    import os
    for key in list(os.environ.keys()):
        if key.startswith("RUNQ_"):
            monkeypatch.delenv(key, raising=False)
    return monkeypatch
