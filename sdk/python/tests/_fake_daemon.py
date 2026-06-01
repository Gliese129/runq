"""A minimal Unix-socket HTTP server for tests.

This is NOT a polished HTTP server — it just parses enough of the
request to assert SDK behavior, then responds with a configurable
status + body. Threaded so we can run it in-process while pytest's
main thread drives the SDK.

Usage (typical pytest fixture):

    with FakeDaemon(socket_path) as fd:
        fd.queue_response(status=200, body={"frozen": True})
        # ... run SDK code that POSTs to socket_path ...
        assert fd.calls[0]["path"] == "/api/internal/freeze-self"
        assert json.loads(fd.calls[0]["body"]) == {"task_id": "t1", ...}
"""
from __future__ import annotations

import json
import os
import socket
import threading
from dataclasses import dataclass, field
from typing import Any


@dataclass
class Recorded:
    """One captured request."""
    method: str = ""
    path: str = ""
    headers: dict = field(default_factory=dict)
    body: str = ""


@dataclass
class _Response:
    status: int = 200
    reason: str = "OK"
    body: Any = None  # dict → json-encoded; str → as-is; None → empty


class FakeDaemon:
    """Threaded fake HTTP daemon over a Unix socket.

    - Spin up at ``__enter__``, tear down at ``__exit__``.
    - ``queue_response(status, body, reason)`` adds one response to the
      reply queue; the next request consumes it. If the queue is empty,
      replies 500 with an "unconfigured" message — that's a hint to the
      test that it forgot to queue a response.
    - ``calls`` is the list of every request that came in (Recorded
      objects) so tests can inspect what the SDK sent.
    """

    def __init__(self, socket_path: str):
        self.socket_path = socket_path
        self.calls: list[Recorded] = []
        self._responses: list[_Response] = []
        self._lock = threading.Lock()
        self._sock: socket.socket | None = None
        self._thread: threading.Thread | None = None
        self._stop = threading.Event()

    # ---- public API ----

    def queue_response(
        self, status: int = 200, body: Any = None, reason: str = "OK"
    ) -> None:
        with self._lock:
            self._responses.append(_Response(status=status, reason=reason, body=body))

    # ---- lifecycle ----

    def __enter__(self) -> FakeDaemon:
        # Pre-clean any stale socket file (tests reuse tmp paths sometimes).
        try:
            os.unlink(self.socket_path)
        except FileNotFoundError:
            pass
        self._sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self._sock.bind(self.socket_path)
        self._sock.listen(8)
        self._sock.settimeout(0.1)  # so accept() polls the stop flag
        self._thread = threading.Thread(target=self._serve, daemon=True)
        self._thread.start()
        return self

    def __exit__(self, *exc) -> None:
        self._stop.set()
        if self._sock is not None:
            try:
                self._sock.close()
            except OSError:
                pass
        if self._thread is not None:
            self._thread.join(timeout=2.0)
        try:
            os.unlink(self.socket_path)
        except FileNotFoundError:
            pass

    # ---- server loop ----

    def _serve(self) -> None:
        assert self._sock is not None
        while not self._stop.is_set():
            try:
                conn, _ = self._sock.accept()
            except TimeoutError:
                continue
            except OSError:
                # socket closed during shutdown
                return
            try:
                self._handle(conn)
            finally:
                try:
                    conn.close()
                except OSError:
                    pass

    def _handle(self, conn: socket.socket) -> None:
        conn.settimeout(2.0)
        buf = bytearray()
        # Read headers (everything before \r\n\r\n), then body by
        # Content-Length. Robust enough for one request per connection
        # which is all the SDK does.
        while b"\r\n\r\n" not in buf:
            chunk = conn.recv(4096)
            if not chunk:
                return  # client gave up
            buf.extend(chunk)

        head_end = buf.index(b"\r\n\r\n")
        head = buf[:head_end].decode("iso-8859-1")
        rest = buf[head_end + 4:]

        # Parse request line.
        lines = head.split("\r\n")
        method, path, _ = lines[0].split(" ", 2)
        headers: dict[str, str] = {}
        for line in lines[1:]:
            if ":" in line:
                k, v = line.split(":", 1)
                headers[k.strip().lower()] = v.strip()

        # Read body if Content-Length says so.
        body_bytes = bytearray(rest)
        content_length = int(headers.get("content-length", "0"))
        while len(body_bytes) < content_length:
            chunk = conn.recv(min(4096, content_length - len(body_bytes)))
            if not chunk:
                break
            body_bytes.extend(chunk)

        recorded = Recorded(
            method=method,
            path=path,
            headers=headers,
            body=bytes(body_bytes).decode("utf-8"),
        )
        with self._lock:
            self.calls.append(recorded)
            if self._responses:
                resp = self._responses.pop(0)
            else:
                resp = _Response(
                    status=500,
                    reason="Unconfigured",
                    body={"error": "FakeDaemon got an unexpected request"},
                )

        # Build response.
        if resp.body is None:
            resp_body_bytes = b""
        elif isinstance(resp.body, (dict, list)):
            resp_body_bytes = json.dumps(resp.body).encode("utf-8")
        else:
            resp_body_bytes = str(resp.body).encode("utf-8")

        resp_head = (
            f"HTTP/1.1 {resp.status} {resp.reason}\r\n"
            f"Content-Type: application/json\r\n"
            f"Content-Length: {len(resp_body_bytes)}\r\n"
            f"Connection: close\r\n"
            f"\r\n"
        ).encode("ascii")
        try:
            conn.sendall(resp_head + resp_body_bytes)
        except OSError:
            pass  # client went away; nothing we can do
