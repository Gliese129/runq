"""Tests for runq.utils.atomic_write — atomicity, error cleanup, modes."""
import json
import os

import pytest

import runq


def test_atomic_write_text(tmp_path):
    target = tmp_path / "out.json"
    with runq.utils.atomic_write(target) as f:
        json.dump({"a": 1}, f)
    assert json.loads(target.read_text()) == {"a": 1}


def test_atomic_write_binary(tmp_path):
    target = tmp_path / "blob.bin"
    with runq.utils.atomic_write(target, mode="wb") as f:
        f.write(b"\x00\x01\x02")
    assert target.read_bytes() == b"\x00\x01\x02"


def test_atomic_write_replaces_existing(tmp_path):
    target = tmp_path / "out.txt"
    target.write_text("old")
    with runq.utils.atomic_write(target) as f:
        f.write("new")
    assert target.read_text() == "new"


def test_atomic_write_error_leaves_target_untouched(tmp_path):
    target = tmp_path / "out.txt"
    target.write_text("old")
    with pytest.raises(RuntimeError):
        with runq.utils.atomic_write(target) as f:
            f.write("partial")
            raise RuntimeError("boom")
    assert target.read_text() == "old"


def test_atomic_write_error_removes_temp_file(tmp_path):
    target = tmp_path / "out.txt"
    with pytest.raises(RuntimeError):
        with runq.utils.atomic_write(target) as f:
            f.write("partial")
            raise RuntimeError("boom")
    assert list(tmp_path.iterdir()) == []


def test_atomic_write_no_temp_leftover_on_success(tmp_path):
    target = tmp_path / "out.txt"
    with runq.utils.atomic_write(target) as f:
        f.write("done")
    assert [p.name for p in tmp_path.iterdir()] == ["out.txt"]


def test_atomic_write_rejects_odd_modes(tmp_path):
    with pytest.raises(ValueError, match="mode"):
        with runq.utils.atomic_write(tmp_path / "x", mode="a"):
            pass


def test_atomic_write_relative_path(tmp_path, monkeypatch):
    monkeypatch.chdir(tmp_path)
    with runq.utils.atomic_write("rel.txt") as f:
        f.write("ok")
    assert (tmp_path / "rel.txt").read_text() == "ok"


def test_atomic_write_perms_honor_umask(tmp_path):
    """The result must NOT keep mkstemp's 0600 — plain-open parity."""
    target = tmp_path / "out.txt"
    old_umask = os.umask(0o022)
    try:
        with runq.utils.atomic_write(target) as f:
            f.write("x")
    finally:
        os.umask(old_umask)
    assert (target.stat().st_mode & 0o777) == 0o644
