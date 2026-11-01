"""Shared test plumbing: a running mock, event capture, a raw SMTP client and a log capture."""

from __future__ import annotations

import json
import logging
import queue
import socket
import ssl
import time
import unittest
from email.message import EmailMessage
from email.utils import make_msgid
from typing import Any

from mail_mock import manifest
from mail_mock.app import MailMock
from mail_mock.scope import Scope

from .schema_check import load_event_schema, problems

HOST = "127.0.0.1"
#: How long a test waits for something that must happen, in seconds.
WAIT_SECONDS = 5.0
#: The probe grace the tests run with: short, so a `tcp` scope's SMTP greeting is not slow.
TEST_PROBE_GRACE_MS = 50

_SCHEMA = load_event_schema()


def assert_valid_event(test: unittest.TestCase, event: dict[str, Any]) -> None:
    """Fail the test if `event` breaks the published `mocks.event/v1` schema."""
    found = problems(event, _SCHEMA, _SCHEMA)
    test.assertEqual(found, [], f"event breaks mocks.event/v1: {json.dumps(event)[:300]}")


class EventTap:
    """Collects every event of the mock, validating each against the published schema."""

    def __init__(self, test: unittest.TestCase, mock: MailMock, run_id: str | None = None) -> None:
        self._test = test
        self._sub = mock.bus.subscribe(run_id)
        self.events: list[dict[str, Any]] = []

    def drain(self) -> list[dict[str, Any]]:
        while True:
            try:
                line = self._sub.queue.get_nowait()
            except queue.Empty:
                return self.events
            event = json.loads(line)
            assert_valid_event(self._test, event)
            self.events.append(event)

    def raw_text(self) -> str:
        return "\n".join(json.dumps(e) for e in self.drain())

    def wait_for(self, predicate, what: str) -> dict[str, Any]:
        deadline = time.monotonic() + WAIT_SECONDS
        while True:
            for event in self.drain():
                if predicate(event):
                    return event
            if time.monotonic() > deadline:
                self._test.fail(f"no event arrived within {WAIT_SECONDS}s: {what}")
            time.sleep(0.01)

    def of_kind(self, kind: str) -> list[dict[str, Any]]:
        return [e for e in self.drain() if e["kind"] == kind]


class LogCapture(logging.Handler):
    """Everything the mock logs, as the text a reader of its log would see."""

    def __init__(self) -> None:
        super().__init__(level=logging.DEBUG)
        self.lines: list[str] = []

    def emit(self, record: logging.LogRecord) -> None:
        self.lines.append(self.format(record))

    def text(self) -> str:
        return "\n".join(self.lines)

    def __enter__(self) -> "LogCapture":
        self._logger = logging.getLogger("mail_mock")
        self._old_level = self._logger.level
        self._logger.setLevel(logging.DEBUG)
        self._logger.addHandler(self)
        return self

    def __exit__(self, *exc: object) -> None:
        self._logger.removeHandler(self)
        self._logger.setLevel(self._old_level)


class MockTestCase(unittest.TestCase):
    """A fresh mock per test; scopes are closed by `tearDown`."""

    def setUp(self) -> None:
        self.mock = MailMock(probe_grace_ms=TEST_PROBE_GRACE_MS)
        self.addCleanup(self.mock.shutdown)
        self.tap = EventTap(self, self.mock)

    def open_scope(self, run_id: str, protocol: str = "smtp") -> Scope:
        return self.mock.open_scope({"run_id": run_id, "listener": {"protocol": protocol, "host": HOST, "port": 0}})

    def trust(self, scope: Scope) -> ssl.SSLContext:
        """A client context that trusts exactly this scope's generated certificate."""
        return ssl.create_default_context(cadata=scope.certificate.certificate_pem)

    def set_settings(self, scope: Scope, settings: dict[str, Any]) -> None:
        self.mock.put_settings(scope.run_id, settings)

    @staticmethod
    def message(subject: str = "Order confirmation", to: str = "customer@acme-example.test", body: str = "Thank you.\n") -> EmailMessage:
        msg = EmailMessage()
        msg["From"] = "shop@acme-example.test"
        msg["To"] = to
        msg["Subject"] = subject
        msg["Message-ID"] = make_msgid(domain="acme-example.test")
        msg.set_content(body)
        return msg


class RawSmtp:
    """A byte-exact SMTP client, so a test can break exactly one thing in the dialogue."""

    def __init__(self, port: int, host: str = HOST) -> None:
        self.sock: socket.socket = socket.create_connection((host, port), timeout=WAIT_SECONDS)
        self.file = self.sock.makefile("rb")
        self.greeting = self.read()

    def read(self) -> tuple[int, list[str]]:
        """One full reply: its code and its text lines."""
        lines: list[str] = []
        while True:
            raw = self.file.readline()
            if not raw:
                raise ConnectionError("the server closed the connection")
            text = raw.decode("utf-8").rstrip("\r\n")
            lines.append(text[4:])
            if text[3:4] != "-":
                return int(text[:3]), lines

    def cmd(self, line: str) -> tuple[int, list[str]]:
        self.sock.sendall(line.encode("utf-8") + b"\r\n")
        return self.read()

    def ehlo(self) -> tuple[int, list[str]]:
        return self.cmd("EHLO client.acme-example.test")

    def starttls(self, context: ssl.SSLContext) -> None:
        code, _ = self.cmd("STARTTLS")
        assert code == 220, code
        self.file.close()
        self.sock = context.wrap_socket(self.sock, server_hostname=HOST)
        self.file = self.sock.makefile("rb")

    def close(self) -> None:
        for closer in (self.file.close, self.sock.close):
            try:
                closer()
            except OSError:
                pass  # already closed by the server

    def __enter__(self) -> "RawSmtp":
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()


def plain_credentials() -> tuple[str, str]:
    """The fictional defaults a scope starts with."""
    return manifest.DEFAULT_USER, manifest.DEFAULT_PASSWORD
