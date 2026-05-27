"""Tests for runq._sizing.estimate_size — the safe_save size walker.

torch is required for the interesting cases. Tests that need it use
``pytest.importorskip("torch")``; the no-torch fallback test runs
regardless.
"""
import pytest

from runq._sizing import estimate_size, _PICKLE_OVERHEAD


def test_no_recognized_objects_returns_none():
    """Walker should return None when nothing recognizable was found.

    None is the "couldn't estimate, ask user for size_hint" signal.
    Returning 0 would silently disable the disk check.
    """
    assert estimate_size() is None
    assert estimate_size("a string") is None
    assert estimate_size(42, "x", b"bytes") is None
    assert estimate_size({"k": "v"}, [1, 2, 3]) is None  # nested but no tensors


# ---- torch tests ----

def test_tensor_counts_numel_times_element_size():
    torch = pytest.importorskip("torch")
    # 100 floats × 4 bytes = 400 raw bytes. After pickle overhead × 1.1.
    t = torch.zeros(100, dtype=torch.float32)
    n = estimate_size(t)
    assert n == int(400 * _PICKLE_OVERHEAD)


def test_tensor_double_precision_doubles_size():
    torch = pytest.importorskip("torch")
    # 100 doubles × 8 bytes = 800 raw bytes.
    t = torch.zeros(100, dtype=torch.float64)
    n = estimate_size(t)
    assert n == int(800 * _PICKLE_OVERHEAD)


def test_dict_of_tensors_sums():
    torch = pytest.importorskip("torch")
    state = {
        "weight": torch.zeros(50, dtype=torch.float32),   # 200 B
        "bias":   torch.zeros(10, dtype=torch.float32),   # 40 B
    }
    n = estimate_size(state)
    # 240 B × overhead
    assert n == int(240 * _PICKLE_OVERHEAD)


def test_nested_dict_lists_walked():
    torch = pytest.importorskip("torch")
    state = {
        "model": {
            "layer1": torch.zeros(10, dtype=torch.float32),   # 40 B
            "layer2": torch.zeros(10, dtype=torch.float32),   # 40 B
        },
        "optim": [
            torch.zeros(5, dtype=torch.float32),              # 20 B
            torch.zeros(5, dtype=torch.float32),              # 20 B
        ],
        "meta": {"epoch": 5, "note": "ignored"},              # 0 (no tensors)
    }
    n = estimate_size(state)
    assert n == int(120 * _PICKLE_OVERHEAD)


def test_nn_module_walks_params_and_buffers():
    torch = pytest.importorskip("torch")
    # Linear(in=4, out=8) → weight=32 floats + bias=8 floats = 40 × 4B = 160 B
    layer = torch.nn.Linear(4, 8)
    n = estimate_size(layer)
    # The exact param count: 4*8 + 8 = 40 floats × 4 bytes = 160 B
    assert n == int(160 * _PICKLE_OVERHEAD)


def test_module_with_buffer_counts_both():
    torch = pytest.importorskip("torch")
    # BatchNorm has running_mean / running_var buffers.
    bn = torch.nn.BatchNorm1d(10)
    n = estimate_size(bn)
    # 2 params (weight + bias, 10 floats each) = 80 B
    # 2 float buffers (running_mean + running_var, 10 each) = 80 B
    # 1 int64 buffer (num_batches_tracked, 1 element) = 8 B
    # total 168 B raw — verify both params + buffers got counted
    assert n is not None
    assert n >= int(160 * _PICKLE_OVERHEAD)


def test_args_and_kwargs_both_walked():
    """Make sure kwargs aren't ignored — decorator form depends on this."""
    torch = pytest.importorskip("torch")
    a = torch.zeros(10, dtype=torch.float32)     # 40 B
    b = torch.zeros(20, dtype=torch.float32)     # 80 B
    n = estimate_size(a, optim=b)
    assert n == int(120 * _PICKLE_OVERHEAD)


def test_mixed_known_and_unknown():
    """Strings, ints, None should be silently skipped without breaking the walk."""
    torch = pytest.importorskip("torch")
    n = estimate_size(
        torch.zeros(10, dtype=torch.float32),  # 40 B
        "ignored",
        42,
        None,
        kw=torch.zeros(5, dtype=torch.float32),  # 20 B
    )
    assert n == int(60 * _PICKLE_OVERHEAD)
