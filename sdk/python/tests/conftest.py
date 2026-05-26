"""Shared pytest fixtures for the runq SDK test suite.

Most tests need a fresh singleton state — the SDK keeps a module-level
Context which would leak between tests. The autouse `_reset_sdk`
fixture below wipes both the context singleton and the auto-step
counter before every test.
"""
import pytest

from runq import _context, _events


@pytest.fixture(autouse=True)
def _reset_sdk():
    """Reset all SDK module-level state before each test."""
    _context._reset_for_tests()
    _events._reset_for_tests()
    yield
    _context._reset_for_tests()
    _events._reset_for_tests()


@pytest.fixture
def clean_env(monkeypatch):
    """Clear every RUNQ_* env var so tests start from a known blank slate."""
    import os
    for key in list(os.environ.keys()):
        if key.startswith("RUNQ_"):
            monkeypatch.delenv(key, raising=False)
    return monkeypatch
