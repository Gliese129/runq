"""Tests for runq._transport.post_json.

These tests stand up a real Unix-socket HTTP server (see _fake_daemon.py)
and assert that the SDK's hand-rolled HTTP framing is correct end-to-end.
No mocking of HTTP — we want to catch real wire-format bugs.

Tests are written against the public contract in the post_json docstring.
They WILL fail until you implement the body — that's the point.
"""
import json
import os
import socket
import threading

import pytest

from runq._transport import TransportError, post_json
from tests._fake_daemon import FakeDaemon


@pytest.fixture
def sock_path():
    """A short path for AF_UNIX sockets.

    Pytest's ``tmp_path`` lives under a long sandbox-specific prefix
    that can exceed AF_UNIX's 108-char limit on Linux. We allocate a
    short path under ``/tmp`` manually and clean up after the test.
    """
    import tempfile
    fd, path = tempfile.mkstemp(prefix="runq-t-", suffix=".sock", dir="/tmp")
    os.close(fd)
    os.unlink(path)  # so the socket can bind to it
    yield path
    try:
        os.unlink(path)
    except FileNotFoundError:
        pass


# ---- happy path ----

def test_post_json_200(sock_path):
    """Round-trip a typical freeze-self request and a 200 response."""
    with FakeDaemon(sock_path) as fd:
        fd.queue_response(status=200, body={"frozen": True})
        result = post_json(
            sock_path,
            "/api/internal/freeze-self",
            {"task_id": "t1", "free_bytes": 100, "needed_est": 2000, "mount": "/data"},
        )
        assert result == {"frozen": True}

    # Verify what the SDK sent.
    assert len(fd.calls) == 1
    call = fd.calls[0]
    assert call.method == "POST"
    assert call.path == "/api/internal/freeze-self"
    assert call.headers["content-type"] == "application/json"
    assert "host" in call.headers          # gin requires Host header
    # No assertion on Connection header — httpx defaults to keep-alive
    # and closes the socket via Client.__exit__ which gives the daemon
    # the same EOF semantics. Either value is fine.
    assert json.loads(call.body) == {
        "task_id": "t1",
        "free_bytes": 100,
        "needed_est": 2000,
        "mount": "/data",
    }


def test_post_json_200_empty_body(sock_path):
    """Daemon responses with empty body should decode to empty dict, not error."""
    with FakeDaemon(sock_path) as fd:
        fd.queue_response(status=200, body=None)
        result = post_json(sock_path, "/x", {"a": 1})
        assert result == {}


def test_content_length_set_correctly(sock_path):
    """Content-Length header must match the actual body length in bytes.

    Use multi-byte UTF-8 ("世界") to catch len-vs-bytes bugs. We compare
    Content-Length to whatever bytes the daemon actually received, not
    to our own json.dumps result — httpx may serialize with
    ensure_ascii=False (raw UTF-8 bytes) while default json.dumps
    escapes to \\uXXXX. Either is valid; the invariant is that
    Content-Length matches what's on the wire.
    """
    with FakeDaemon(sock_path) as fd:
        fd.queue_response(status=200, body={"ok": 1})
        post_json(sock_path, "/x", {"hello": "世界"})

        sent = fd.calls[0]
        actual_bytes = len(sent.body.encode("utf-8"))
        assert int(sent.headers["content-length"]) == actual_bytes


# ---- error paths ----

def test_no_socket_file_raises_transport_error():
    """No daemon listening → TransportError with status=None."""
    # Short path under /tmp so AF_UNIX accepts it even though the file
    # doesn't exist (the path-length check fires regardless of presence).
    nonexistent = "/tmp/runq-t-nope.sock"
    with pytest.raises(TransportError) as ei:
        post_json(nonexistent, "/x", {"a": 1}, timeout=1.0)
    assert ei.value.status is None  # connection error, not HTTP response


def test_daemon_500_raises_transport_error(sock_path):
    """Daemon returned 500 → TransportError with status=500 + body."""
    with FakeDaemon(sock_path) as fd:
        fd.queue_response(status=500, body={"error": "boom"})
        with pytest.raises(TransportError) as ei:
            post_json(sock_path, "/x", {"a": 1})
        assert ei.value.status == 500
        assert "boom" in ei.value.body


def test_daemon_400_propagates_status_and_body(sock_path):
    """4xx is also a TransportError — caller branches on status."""
    with FakeDaemon(sock_path) as fd:
        fd.queue_response(status=400, body={"error": "task not running"})
        with pytest.raises(TransportError) as ei:
            post_json(sock_path, "/api/internal/freeze-self", {})
        assert ei.value.status == 400
        assert "task not running" in ei.value.body


def test_daemon_responds_non_json(sock_path):
    """200 with garbage body should raise (we promise to return a dict)."""
    with FakeDaemon(sock_path) as fd:
        fd.queue_response(status=200, body="this is not json")
        with pytest.raises(TransportError) as ei:
            post_json(sock_path, "/x", {})
        # Status is the HTTP status even though decoding failed.
        # Body is the raw non-JSON string so the caller can debug.
        assert "this is not json" in ei.value.body


# ---- timeout / hanging connections ----

def test_short_timeout_aborts(sock_path):
    """Caller can pass a short timeout to avoid blocking forever on a hung daemon.

    We use a custom blocking server that never replies. The SDK should
    raise TransportError within the timeout window.
    """
    stop = threading.Event()

    def silent_server():
        s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        try:
            s.bind(sock_path)
            s.listen(1)
            s.settimeout(0.1)
            while not stop.is_set():
                try:
                    conn, _ = s.accept()
                except socket.timeout:
                    continue
                # Accept but never respond — until we're asked to stop.
                while not stop.is_set():
                    stop.wait(0.05)
                try:
                    conn.close()
                except OSError:
                    pass
        finally:
            s.close()
            try:
                os.unlink(sock_path)
            except FileNotFoundError:
                pass

    t = threading.Thread(target=silent_server, daemon=True)
    t.start()
    # Give the server a moment to bind.
    for _ in range(50):
        if os.path.exists(sock_path):
            break
        import time
        time.sleep(0.02)

    try:
        with pytest.raises(TransportError):
            post_json(sock_path, "/x", {"a": 1}, timeout=0.5)
    finally:
        stop.set()
        t.join(timeout=2.0)


# ---- threading / concurrent calls ----

def test_concurrent_calls_dont_collide(sock_path):
    """Two threads POSTing simultaneously should both succeed and get
    distinct responses (one socket per call, no shared state)."""
    with FakeDaemon(sock_path) as fd:
        fd.queue_response(status=200, body={"id": 1})
        fd.queue_response(status=200, body={"id": 2})

        results: list[dict] = []
        errors: list[Exception] = []

        def go():
            try:
                results.append(post_json(sock_path, "/x", {"go": True}))
            except Exception as e:
                errors.append(e)

        threads = [threading.Thread(target=go) for _ in range(2)]
        for t in threads:
            t.start()
        for t in threads:
            t.join(timeout=5.0)

        assert not errors, f"some calls failed: {errors}"
        assert len(results) == 2
        # IDs come back in some order — both should appear.
        assert {1, 2} == {r["id"] for r in results}
