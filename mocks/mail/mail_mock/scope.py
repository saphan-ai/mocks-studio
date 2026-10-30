"""One run's scope: its settings, its mailbox, its SMTP and IMAP listeners, its events."""

from __future__ import annotations

import logging
import threading
from typing import TYPE_CHECKING, Any

from . import manifest
from .events import correlation_id_for_key, new_correlation_id, redacted_payload
from .imap import ImapSession
from .mailbox import Mailbox
from .net import Listener
from .smtp import SmtpSession
from .tlsutil import ScopeCertificate

if TYPE_CHECKING:
    from .app import MailMock

log = logging.getLogger("mail_mock")


class Refusal(Exception):
    """A refusal of a control request: HTTP status plus the Refusal document of the spec."""

    def __init__(self, status: int, reason_code: str, message: str, allowed: list[Any] | None = None) -> None:
        super().__init__(message)
        self.status = status
        self.reason_code = reason_code
        self.message = message
        self.allowed = allowed

    def document(self) -> dict[str, Any]:
        doc: dict[str, Any] = {"message": self.message, "reason_code": self.reason_code}
        if self.allowed is not None:
            doc["allowed"] = self.allowed
        return doc


def _declared() -> dict[str, dict[str, Any]]:
    return {s["name"]: s for s in manifest.settings()}


def _type_ok(declared: dict[str, Any], value: Any) -> bool:
    kind = declared["type"]
    if kind == "boolean":
        return isinstance(value, bool)
    if isinstance(value, bool):
        return False  # a JSON true is not a number
    if kind == "integer":
        return isinstance(value, int) or (isinstance(value, float) and value.is_integer())
    if kind == "number":
        return isinstance(value, (int, float))
    return isinstance(value, str)


def validate_settings(request: Any) -> dict[str, Any]:
    """Check a whole settings request; raise the first refusal, apply nothing."""
    declared = _declared()
    if not isinstance(request, dict):
        raise Refusal(
            400, "request-invalid",
            "The settings request body is not a JSON object, so nothing was applied; send an object of setting name to value.",
        )
    for name in request:
        if name not in declared:
            raise Refusal(
                422, "setting-unknown",
                f'The setting "{name}" is not declared by this mock and the whole request was refused; use one of the allowed settings.',
                sorted(declared),
            )
    clean: dict[str, Any] = {}
    for name, value in request.items():
        spec = declared[name]
        if not _type_ok(spec, value):
            raise Refusal(
                422, "setting-type-invalid",
                f'The setting "{name}" takes a {spec["type"]} and the whole request was refused; send a value of that type.',
            )
        if spec["type"] == "integer":
            value = int(value)
        problem = _range_problem(spec, value)
        if problem:
            raise Refusal(422, "setting-out-of-range", f'The setting "{name}" {problem[0]} and the whole request was refused; {problem[1]}.', problem[2])
        clean[name] = value
    return clean


def _range_problem(spec: dict[str, Any], value: Any) -> tuple[str, str, list[Any] | None] | None:
    if "allowed" in spec and value not in spec["allowed"]:
        return "does not accept that value", "use one of the allowed values", list(spec["allowed"])
    if isinstance(value, (int, float)) and not isinstance(value, bool):
        lo, hi = spec.get("minimum"), spec.get("maximum")
        if (lo is not None and value < lo) or (hi is not None and value > hi):
            return (f"takes a value from {lo} to {hi}", f"send a value inside that range", None)
    return None


class Scope:
    """What the mock keeps for one run. Two scopes share nothing."""

    def __init__(self, mock: "MailMock", run_id: str, protocol: str, host: str, port: int) -> None:
        self.mock = mock
        self.run_id = run_id
        self.protocol = protocol
        self.host = host
        self._lock = threading.Lock()
        self._settings = {s["name"]: s["default"] for s in manifest.settings()}
        self.mailbox = Mailbox()
        self.closing = threading.Event()
        self.certificate = ScopeCertificate()
        self._smtp: Listener | None = None
        self._imap: Listener | None = None
        self._open_listeners(host, port)

    def _open_listeners(self, host: str, port: int) -> None:
        """Bind both listeners before returning; a failure leaves none open."""
        self._smtp = Listener(host, port, lambda conn, peer: SmtpSession(self, conn, peer).run())
        try:
            self._imap = Listener(host, 0, lambda conn, peer: ImapSession(self, conn, peer).run())
        except OSError:
            self._smtp.stop()
            raise
        self.port = self._smtp.port
        self.imap_port = self._imap.port

    def track_smtp_connection(self, conn: Any) -> None:
        """Let a close of the scope cut a connection the SMTP side upgraded to TLS."""
        if self._smtp is not None:
            self._smtp.track(conn)

    # -- settings ---------------------------------------------------------------------------

    def get(self, name: str) -> Any:
        with self._lock:
            return self._settings[name]

    def all_settings(self) -> dict[str, Any]:
        with self._lock:
            return dict(self._settings)

    def apply(self, clean: dict[str, Any]) -> dict[str, Any]:
        with self._lock:
            self._settings.update(clean)
            current = dict(self._settings)
        self.emit(
            "scenario.set", "internal",
            "settings set: " + ", ".join(sorted(clean)),  # names only; a value may be a credential
            payload=redacted_payload(",".join(sorted(clean))),
        )
        return current

    def reset(self) -> None:
        with self._lock:
            self._settings = {s["name"]: s["default"] for s in manifest.settings()}
        self.emit("scope.reset", "internal", "per-run settings returned to their defaults")

    # -- events -----------------------------------------------------------------------------

    def emit(self, kind: str, direction: str, summary: str, *, protocol: str | None = None, **kw: Any) -> dict[str, Any]:
        return self.mock.bus.emit(self.run_id, kind, direction, protocol or self.protocol, summary, **kw)

    def correlation_for_message(self, message_id: str) -> str:
        return correlation_id_for_key(message_id)

    @staticmethod
    def new_correlation() -> str:
        return new_correlation_id()

    # -- lifecycle --------------------------------------------------------------------------

    def description(self) -> dict[str, Any]:
        """The 201 body: the listener as assigned, plus where this scope's IMAP side and certificate are."""
        return {
            "run_id": self.run_id,
            "listener": {"protocol": self.protocol, "host": self.host, "port": self.port},
            "imap": {"host": self.host, "port": self.imap_port},
            "tls_certificate_pem": self.certificate.certificate_pem,
        }

    def close(self) -> None:
        """Stop answering on both listeners, then cut open sessions."""
        self.closing.set()
        for listener in (self._smtp, self._imap):
            if listener is not None:
                listener.stop()
