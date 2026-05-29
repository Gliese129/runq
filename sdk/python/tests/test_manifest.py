"""Tests for ``runq._manifest`` — the checkpoint ledger backing
``keep_last_n`` / ``keep_best``.

These tests exercise the module directly (no safe_save involvement)
so the failure mode is sharp when the cleanup core misbehaves. The
integration with safe_save lives in ``test_safe_save_cleanup.py``.

Key invariants tested:
- Atomic round-trip (load → save → load).
- Missing / corrupt / wrong-schema files start fresh instead of
  raising into the user's task.
- ``append_entry`` honors the single-best rule: only the new entry
  keeps ``is_best=True`` after append.
- ``cleanup`` is manifest-scoped: sibling files in checkpoint_dir that
  AREN'T tracked must survive every policy combination.
- ``cleanup`` orders by ``(step, ts)`` desc — same-ts entries are
  broken by step.
- ``keep_best`` rescues the best-flagged entry from ``keep_last_n``
  eviction.
"""
import json
import os
from pathlib import Path

import pytest

from runq import _manifest

# ---- helpers --------------------------------------------------------

def _read_raw(checkpoint_dir: Path) -> dict:
    """Read the manifest file directly off disk (bypasses load_manifest)."""
    return json.loads((checkpoint_dir / _manifest.MANIFEST_FILENAME).read_text())


def _entry(path: str, *, step=None, is_best=False, size_bytes=100, ts=0) -> dict:
    return {
        "path": path,
        "step": step,
        "is_best": is_best,
        "size_bytes": size_bytes,
        "ts": ts,
    }


# ---- mechanical: load + save round-trip ----------------------------

def test_manifest_path_helper(tmp_path):
    assert _manifest.manifest_path(tmp_path) == tmp_path / ".runq_manifest.json"


def test_load_when_missing_returns_empty(tmp_path):
    """First save: manifest doesn't exist yet. Must return empty, not crash."""
    m = _manifest.load_manifest(tmp_path)
    assert m == {"version": _manifest.MANIFEST_VERSION, "entries": []}


def test_save_then_load_round_trip(tmp_path):
    m_in = {
        "version": _manifest.MANIFEST_VERSION,
        "entries": [_entry("a.pt", step=1, ts=10), _entry("b.pt", step=2, ts=20)],
    }
    _manifest.save_manifest(tmp_path, m_in)
    m_out = _manifest.load_manifest(tmp_path)
    assert m_out == m_in


def test_save_is_atomic_no_tmp_file_left_behind(tmp_path):
    _manifest.save_manifest(tmp_path, {"version": 1, "entries": []})
    leftovers = [p for p in tmp_path.iterdir() if ".tmp-" in p.name]
    assert leftovers == [], f"atomic write leaked tmp files: {leftovers}"


def test_load_corrupt_json_starts_fresh(tmp_path, caplog):
    """Corrupt manifest must not crash; should log + return empty."""
    (tmp_path / _manifest.MANIFEST_FILENAME).write_text("{not json")
    m = _manifest.load_manifest(tmp_path)
    assert m["entries"] == []
    assert m["version"] == _manifest.MANIFEST_VERSION


def test_load_wrong_version_starts_fresh(tmp_path):
    bogus = {"version": 999, "entries": [_entry("a.pt")]}
    (tmp_path / _manifest.MANIFEST_FILENAME).write_text(json.dumps(bogus))
    m = _manifest.load_manifest(tmp_path)
    assert m["entries"] == []


def test_load_non_dict_starts_fresh(tmp_path):
    (tmp_path / _manifest.MANIFEST_FILENAME).write_text(json.dumps(["not", "a", "dict"]))
    m = _manifest.load_manifest(tmp_path)
    assert m["entries"] == []


def test_load_missing_entries_field_starts_fresh(tmp_path):
    (tmp_path / _manifest.MANIFEST_FILENAME).write_text(json.dumps({"version": 1}))
    m = _manifest.load_manifest(tmp_path)
    assert m["entries"] == []


# ---- path key helper ----------------------------------------------

def test_to_manifest_key_under_dir(tmp_path):
    target = tmp_path / "ckpt.pt"
    target.write_text("x")
    assert _manifest.to_manifest_key(tmp_path, target) == "ckpt.pt"


def test_to_manifest_key_nested(tmp_path):
    (tmp_path / "sub").mkdir()
    target = tmp_path / "sub" / "ckpt.pt"
    target.write_text("x")
    assert _manifest.to_manifest_key(tmp_path, target) == os.path.join("sub", "ckpt.pt")


def test_to_manifest_key_outside_returns_none(tmp_path):
    """Absolute path outside checkpoint_dir → None (not runq-managed)."""
    elsewhere = tmp_path.parent / "outside.pt"
    elsewhere.write_text("x")
    try:
        assert _manifest.to_manifest_key(tmp_path, elsewhere) is None
    finally:
        elsewhere.unlink(missing_ok=True)


# ---- append_entry: single-best invariant --------------------------

def test_append_entry_basic(tmp_path):
    """First append creates the file and adds one entry."""
    _manifest.append_entry(tmp_path, _entry("ckpt-1.pt", step=1, ts=10))
    m = _manifest.load_manifest(tmp_path)
    assert len(m["entries"]) == 1
    assert m["entries"][0]["path"] == "ckpt-1.pt"


def test_append_entry_appends_in_order(tmp_path):
    _manifest.append_entry(tmp_path, _entry("a.pt", step=1, ts=10))
    _manifest.append_entry(tmp_path, _entry("b.pt", step=2, ts=20))
    _manifest.append_entry(tmp_path, _entry("c.pt", step=3, ts=30))
    m = _manifest.load_manifest(tmp_path)
    assert [e["path"] for e in m["entries"]] == ["a.pt", "b.pt", "c.pt"]


def test_append_entry_is_best_clears_prior_bests(tmp_path):
    """Single-best invariant — older bests demote when a new best appends."""
    _manifest.append_entry(tmp_path, _entry("a.pt", step=1, ts=10, is_best=True))
    _manifest.append_entry(tmp_path, _entry("b.pt", step=2, ts=20, is_best=False))
    _manifest.append_entry(tmp_path, _entry("c.pt", step=3, ts=30, is_best=True))

    m = _manifest.load_manifest(tmp_path)
    flags = [e["is_best"] for e in m["entries"]]
    # Only the most-recent best stays true.
    assert flags == [False, False, True], f"flags={flags}"


def test_append_entry_non_best_preserves_prior_best(tmp_path):
    """Appending a non-best entry must NOT clear the existing best."""
    _manifest.append_entry(tmp_path, _entry("a.pt", step=1, ts=10, is_best=True))
    _manifest.append_entry(tmp_path, _entry("b.pt", step=2, ts=20, is_best=False))
    m = _manifest.load_manifest(tmp_path)
    flags = [e["is_best"] for e in m["entries"]]
    assert flags == [True, False]


def test_append_entry_returns_updated_manifest(tmp_path):
    m = _manifest.append_entry(tmp_path, _entry("a.pt", step=1, ts=10))
    assert isinstance(m, dict)
    assert m["entries"][-1]["path"] == "a.pt"


# ---- cleanup: keep_last_n ordering --------------------------------

def _populate(tmp_path, specs):
    """Create files + append entries together."""
    for s in specs:
        (tmp_path / s["path"]).write_text("x")
        _manifest.append_entry(tmp_path, s)


def test_cleanup_keep_last_n_simple(tmp_path):
    """5 saves, keep_last_n=3 → 2 oldest by (step, ts) deleted."""
    _populate(tmp_path, [
        _entry("ck1.pt", step=1, ts=10),
        _entry("ck2.pt", step=2, ts=20),
        _entry("ck3.pt", step=3, ts=30),
        _entry("ck4.pt", step=4, ts=40),
        _entry("ck5.pt", step=5, ts=50),
    ])
    deleted = _manifest.cleanup(tmp_path, keep_last_n=3)
    survivors = [p.name for p in tmp_path.iterdir() if p.name != _manifest.MANIFEST_FILENAME]
    assert sorted(survivors) == ["ck3.pt", "ck4.pt", "ck5.pt"]
    # The deleted return value tells callers which absolute paths went away.
    assert sorted(Path(p).name for p in deleted) == ["ck1.pt", "ck2.pt"]

    # And the manifest only mentions survivors now.
    m = _manifest.load_manifest(tmp_path)
    assert sorted(e["path"] for e in m["entries"]) == ["ck3.pt", "ck4.pt", "ck5.pt"]


def test_cleanup_keep_last_n_none_is_no_op(tmp_path):
    _populate(tmp_path, [
        _entry("a.pt", step=1, ts=10),
        _entry("b.pt", step=2, ts=20),
    ])
    deleted = _manifest.cleanup(tmp_path, keep_last_n=None)
    assert deleted == []
    assert sorted(p.name for p in tmp_path.iterdir()) == [
        ".runq_manifest.json", "a.pt", "b.pt",
    ]


def test_cleanup_keep_last_n_more_than_entries_keeps_all(tmp_path):
    _populate(tmp_path, [
        _entry("a.pt", step=1, ts=10),
        _entry("b.pt", step=2, ts=20),
    ])
    deleted = _manifest.cleanup(tmp_path, keep_last_n=10)
    assert deleted == []


def test_cleanup_ordering_uses_step_then_ts(tmp_path):
    """Same-step entries break by ts; otherwise step dominates."""
    _populate(tmp_path, [
        _entry("a.pt", step=2, ts=10),
        _entry("b.pt", step=1, ts=999),  # higher ts but lower step → older
        _entry("c.pt", step=3, ts=5),
    ])
    _manifest.cleanup(tmp_path, keep_last_n=2)
    survivors = sorted(p.name for p in tmp_path.iterdir() if p.name.endswith(".pt"))
    # By (step, ts) desc → c (step3), a (step2), b (step1) → keep c+a.
    assert survivors == ["a.pt", "c.pt"]


# ---- cleanup: policy validation -----------------------------------

def test_cleanup_keep_best_without_keep_last_n_raises(tmp_path):
    """keep_best=True alone is ambiguous and must be rejected.

    Reading 1: 'only the best, drop everything else'
    Reading 2: 'no quantity cap + ensure best is kept = no-op'

    Either way it's a footgun. Force the user to spell it out
    (keep_last_n=0 + keep_best=True for best-only).
    """
    _populate(tmp_path, [_entry("a.pt", step=1, ts=10, is_best=True)])
    with pytest.raises(ValueError, match="keep_last_n"):
        _manifest.cleanup(tmp_path, keep_best=True)


def test_cleanup_negative_keep_last_n_raises(tmp_path):
    with pytest.raises(ValueError, match="keep_last_n must be >= 0"):
        _manifest.cleanup(tmp_path, keep_last_n=-1)


def test_validate_policy_helper(tmp_path):
    """The validator is also reachable on its own (used by safe_save)."""
    # Valid: None alone, 0 with best, N alone, N with best.
    _manifest.validate_policy(None, False)
    _manifest.validate_policy(0, True)
    _manifest.validate_policy(5, False)
    _manifest.validate_policy(5, True)
    # Invalid:
    with pytest.raises(ValueError):
        _manifest.validate_policy(None, True)
    with pytest.raises(ValueError):
        _manifest.validate_policy(-1, False)


# ---- cleanup: keep_best -------------------------------------------

def test_cleanup_keep_last_n_zero_with_keep_best_keeps_only_best(tmp_path):
    """The user-explicit 'best only' spelling: N=0 + keep_best=True."""
    _populate(tmp_path, [
        _entry("a.pt", step=1, ts=10, is_best=False),
        _entry("b.pt", step=2, ts=20, is_best=True),
        _entry("c.pt", step=3, ts=30, is_best=False),
    ])
    _manifest.cleanup(tmp_path, keep_last_n=0, keep_best=True)
    survivors = sorted(p.name for p in tmp_path.iterdir() if p.name.endswith(".pt"))
    assert survivors == ["b.pt"]


def test_cleanup_keep_last_n_zero_without_keep_best_evicts_all(tmp_path):
    """N=0 alone is allowed and means 'rotate the slot, no history'."""
    _populate(tmp_path, [
        _entry("a.pt", step=1, ts=10),
        _entry("b.pt", step=2, ts=20),
    ])
    deleted = _manifest.cleanup(tmp_path, keep_last_n=0)
    assert sorted(Path(p).name for p in deleted) == ["a.pt", "b.pt"]
    survivors = [p.name for p in tmp_path.iterdir() if p.name.endswith(".pt")]
    assert survivors == []
    # Manifest also drained.
    assert _manifest.load_manifest(tmp_path)["entries"] == []


def test_cleanup_keep_best_rescues_from_keep_last_n(tmp_path):
    """The best entry must survive even when keep_last_n would evict it."""
    _populate(tmp_path, [
        _entry("a.pt", step=1, ts=10, is_best=True),
        _entry("b.pt", step=2, ts=20),
        _entry("c.pt", step=3, ts=30),
        _entry("d.pt", step=4, ts=40),
    ])
    _manifest.cleanup(tmp_path, keep_last_n=2, keep_best=True)
    survivors = sorted(p.name for p in tmp_path.iterdir() if p.name.endswith(".pt"))
    # keep_last_n=2 → c, d. keep_best rescues a. b loses.
    assert survivors == ["a.pt", "c.pt", "d.pt"]


def test_cleanup_keep_best_no_best_entry(tmp_path):
    """keep_best with no entry flagged best → falls back to keep_last_n alone."""
    _populate(tmp_path, [
        _entry("a.pt", step=1, ts=10),
        _entry("b.pt", step=2, ts=20),
    ])
    _manifest.cleanup(tmp_path, keep_last_n=1, keep_best=True)
    survivors = sorted(p.name for p in tmp_path.iterdir() if p.name.endswith(".pt"))
    assert survivors == ["b.pt"]


# ---- cleanup: manifest-scoping safety -----------------------------

def test_cleanup_never_touches_untracked_files(tmp_path):
    """The whole point of the manifest: don't delete what we didn't write.

    Lab users put configs, plot PDFs, manual snapshots alongside runq's
    checkpoints. Cleanup must never look at them.
    """
    # Tracked checkpoints.
    _populate(tmp_path, [
        _entry("ck1.pt", step=1, ts=10),
        _entry("ck2.pt", step=2, ts=20),
        _entry("ck3.pt", step=3, ts=30),
    ])
    # User-placed siblings (NOT in manifest).
    (tmp_path / "config.yaml").write_text("lr: 1e-4")
    (tmp_path / "plot.pdf").write_bytes(b"%PDF-fake")
    (tmp_path / "manual_snapshot.pt").write_text("user-saved")

    _manifest.cleanup(tmp_path, keep_last_n=1)

    # Untracked siblings survive — that's the invariant.
    assert (tmp_path / "config.yaml").exists()
    assert (tmp_path / "plot.pdf").exists()
    assert (tmp_path / "manual_snapshot.pt").exists()
    # Tracked ones get the policy.
    assert not (tmp_path / "ck1.pt").exists()
    assert not (tmp_path / "ck2.pt").exists()
    assert (tmp_path / "ck3.pt").exists()


def test_cleanup_missing_file_still_updates_manifest(tmp_path):
    """If a manifest entry's file was already removed externally, cleanup
    should not crash. It should still rewrite the manifest to drop the
    stale entry."""
    _manifest.append_entry(tmp_path, _entry("ghost.pt", step=1, ts=10))
    _manifest.append_entry(tmp_path, _entry("alive.pt", step=2, ts=20))
    (tmp_path / "alive.pt").write_text("x")
    # ghost.pt was never created on disk; cleanup must tolerate this.
    _manifest.cleanup(tmp_path, keep_last_n=1)
    m = _manifest.load_manifest(tmp_path)
    assert [e["path"] for e in m["entries"]] == ["alive.pt"]


def test_cleanup_empty_manifest_is_no_op(tmp_path):
    deleted = _manifest.cleanup(tmp_path, keep_last_n=3, keep_best=True)
    assert deleted == []
