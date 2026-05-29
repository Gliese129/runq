"""HTTP client over a Unix domain socket — thin wrapper over httpx.

Why httpx
---------
httpx natively supports Unix sockets via ``httpx.HTTPTransport(uds=...)``
and handles all the HTTP/1.1 framing pitfalls (Content-Length vs
Transfer-Encoding, header parsing, retry on connection close, etc.) we
don't want to babysit.

It's a dep we accept on purpose: lab user envs almost always have it
already (transitively via wandb / fastapi / transformers), and the
~30 lines of hand-rolled HTTP it would replace are easy to get subtly
wrong.

Daemon contract
---------------
The daemon's HTTP server (gin) speaks normal HTTP/1.1 over the socket
file at ``RUNQ_SOCKET_PATH``. Endpoints relevant to the SDK:

    POST /api/internal/freeze-self      body: {"task_id":...,"free_bytes":...,
                                               "needed_est":...,"mount":...}
                                        → 200 {"frozen": true}
                                        → 400 / 404 on validation errors

The SDK doesn't call any other endpoint from inside a task. ``runq
thaw`` and friends live in the CLI, not here.

Error model
-----------
Any failure to reach the daemon, or any non-2xx response, raises
``TransportError`` so callers (``safe_save``, future SDK features) can
distinguish "daemon down" from "daemon said no".

Threading
---------
Each call opens + closes its own ``httpx.Client``. Safe to call from
multiple threads concurrently. We do not share clients or pool sockets
across calls; the daemon's freeze flow makes connection lifetime
unpredictable so per-call clients are simpler to reason about.
"""
from __future__ import annotations

import json
from typing import Any

import httpx


class TransportError(Exception):
    """Raised when the SDK can't talk to the daemon, or the daemon
    responded with a non-2xx status.

    Carries ``status`` (HTTP status code or ``None`` for connection
    errors) and ``body`` (response body string, may be empty) so callers
    can branch — e.g. ``safe_save`` could log + retry on 503 but exit on
    400 (request was malformed).
    """

    def __init__(
        self,
        message: str,
        *,
        status: int | None = None,
        body: str = "",
    ) -> None:
        super().__init__(message)
        self.status = status
        self.body = body


def post_json(
    socket_path: str,
    path: str,
    body: dict,
    *,
    timeout: float = 30.0,
) -> Any:
    """POST a JSON body to ``path`` on the daemon at ``socket_path``.

    Returns the decoded JSON response on 2xx. Raises ``TransportError``
    on any of: socket connect failure, timeout, non-2xx response, or
    response not parseable as JSON.

    ``timeout`` is the per-call deadline. Default 30s is deliberately
    generous — when SDK calls ``/api/internal/freeze-self`` the daemon
    SIGSTOPs the calling process mid-recv; the connection "hangs" until
    ``runq thaw`` and that's normal. A short timeout would falsely abort
    the freeze cycle. Callers that don't want to block forever (e.g.
    health-check style calls) should pass an explicit shorter timeout.

    """
    transport = httpx.HTTPTransport(uds=socket_path)
    try:
        with httpx.Client(transport=transport, timeout=timeout) as client:
            resp = client.post(f"http://localhost{path}", json=body)
            resp.raise_for_status()
            if not resp.content:
                return {}
            return resp.json()
    except (httpx.ConnectError, httpx.TimeoutException, OSError) as e:
        raise TransportError(f"socket connection error: {e}", status=None) from e
    except httpx.HTTPStatusError as e:
        raise TransportError(
            f"daemon returned {resp.status_code}",
            status=resp.status_code,
            body=resp.text
        ) from e
    except json.JSONDecodeError as e:
        raise TransportError(
            f"json parsed failed: {e}",
            status=resp.status_code, body=resp.text
        ) from e
