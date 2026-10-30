"""The mock itself: the scopes it keeps, the event bus, and the redactor its sessions log through."""

from __future__ import annotations

import logging
import re
import threading
from typing import Any

from . import manifest
from . import redact as redaction
from .control import ControlServer
from .events import EventBus
from .scope import Refusal, Scope, validate_settings

log = logging.getLogger("mail_mock")

RUN_ID = re.compile(r"^[a-z0-9][a-z0-9._-]{2,63}$")

MAX_PORT = 65535


class MailMock:
    """All scopes of one running mock."""

    def __init__(self, *, redactor: redaction.Redactor | None = None, probe_grace_ms: int = manifest.DEFAULT_PROBE_GRACE_MS) -> None:
        self.bus = EventBus()
        self._redactor = redactor
        self.probe_grace_seconds = probe_grace_ms / 1000.0
        self._lock = threading.Lock()
        self._scopes: dict[str, Scope] = {}
        self.control: ControlServer | None = None

    def redactor(self, kind: str, line: str) -> str:
        """The log form of `line`; the default is looked up at call time so a test can replace it."""
        return (self._redactor or redaction.redact)(kind, line)

    def serve_control(self, host: str, port: int) -> ControlServer:
        self.control = ControlServer(self, host, port)
        return self.control

    def scope(self, run_id: str) -> Scope:
        with self._lock:
            found = self._scopes.get(run_id)
        if found is None:
            raise Refusal(
                404, "run-unknown",
                f'No scope is open for run "{run_id}" and nothing was changed; open it with POST /_mock/v1/scopes first.',
            )
        return found

    def open_scope(self, request: Any) -> Scope:
        run_id, protocol, host, port = self._parse_open(request)
        with self._lock:
            if run_id in self._scopes:
                raise Refusal(
                    409, "run-already-open",
                    f'A scope for run "{run_id}" is already open and the new request was refused; close it first or use another run id.',
                )
            try:
                scope = Scope(self, run_id, protocol, host, port)
            except OSError as exc:
                raise Refusal(
                    409, "listener-unavailable",
                    f"The listener {host}:{port} could not be opened ({exc.strerror or type(exc).__name__}) and no scope was created; choose a free port.",
                ) from exc
            self._scopes[run_id] = scope
        log.info("scope %s opened: %s listener on %s:%d, imap on port %d", run_id, protocol, host, scope.port, scope.imap_port)
        scope.emit("scope.opened", "internal", f"scope opened with a {protocol} listener")
        return scope

    @staticmethod
    def _parse_open(request: Any) -> tuple[str, str, str, int]:
        if not isinstance(request, dict):
            raise Refusal(400, "request-invalid", "The open request body is not a JSON object and no scope was created; send run_id and listener.")
        run_id = request.get("run_id")
        if not isinstance(run_id, str) or not RUN_ID.match(run_id):
            raise Refusal(
                422, "run-id-invalid",
                f"The run id {run_id!r} does not match ^[a-z0-9][a-z0-9._-]{{2,63}}$ and no scope was created; use a lower-case run id of 3 to 64 characters.",
            )
        listener = request.get("listener")
        if not isinstance(listener, dict):
            raise Refusal(422, "listener-invalid", "The request carries no listener object and no scope was created; send listener with protocol, host and port.")
        protocol = listener.get("protocol")
        if protocol not in manifest.PROTOCOLS:
            raise Refusal(
                422, "listener-protocol-unsupported",
                f"The listener protocol {protocol!r} is not one this mock serves and no scope was created; use one of the allowed protocols.",
                list(manifest.PROTOCOLS),
            )
        host, port = listener.get("host"), listener.get("port")
        if not isinstance(host, str) or not host or not isinstance(port, int) or isinstance(port, bool) or not 0 <= port <= MAX_PORT:
            raise Refusal(422, "listener-invalid", "The listener needs a host string and an integer port from 0 to 65535 and no scope was created; correct them.")
        return run_id, protocol, host, port

    def close_scope(self, run_id: str) -> None:
        with self._lock:
            scope = self._scopes.pop(run_id, None)
        if scope is None:
            raise Refusal(404, "run-unknown", f'No scope is open for run "{run_id}" and nothing was closed; list nothing, open one first.')
        scope.close()
        log.info("scope %s closed", run_id)
        scope.emit("scope.closed", "internal", "scope closed; its listeners stopped answering")

    def put_settings(self, run_id: str, request: Any) -> dict[str, Any]:
        scope = self.scope(run_id)
        return scope.apply(validate_settings(request))

    def reset(self, run_id: str) -> None:
        self.scope(run_id).reset()

    def shutdown(self) -> None:
        with self._lock:
            run_ids = list(self._scopes)
        for run_id in run_ids:
            try:
                self.close_scope(run_id)
            except Refusal:
                pass  # closed concurrently; nothing left to do
        if self.control is not None:
            self.control.stop()
