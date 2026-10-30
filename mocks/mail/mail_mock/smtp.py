"""The SMTP side (RFC 5321, STARTTLS RFC 3207, AUTH PLAIN and LOGIN RFC 4954), one session per connection."""

from __future__ import annotations

import base64
import binascii
import hmac
import logging
import re
import select
import socket
import ssl
import uuid
from typing import TYPE_CHECKING

from . import manifest
from . import redact as redaction
from .events import clear_payload, redacted_payload
from .mailbox import StoredMail
from .tlsutil import CERT_HOSTNAME

if TYPE_CHECKING:
    from .scope import Scope

log = logging.getLogger("mail_mock")

#: Longest command line, in bytes, RFC 5321 §4.5.3.1.4 (512) widened for AUTH and long addresses.
MAX_LINE_BYTES = 1000
#: A DATA line, CRLF included (RFC 5321 section 4.5.3.1.6).
MAX_DATA_LINE_BYTES = 1000
#: Largest mail accepted, in bytes; advertised in the SIZE extension.
MAX_MESSAGE_BYTES = 10 * 1024 * 1024
#: Most recipients one transaction accepts.
MAX_RECIPIENTS = 100

_MAIL_FROM = re.compile(r"^MAIL\s+FROM:\s*<(?P<addr>[^<>\s]*)>(?P<params>(?:\s+\S+)*)\s*$", re.I)
_RCPT_TO = re.compile(r"^RCPT\s+TO:\s*<(?P<addr>[^<>\s]+)>(?P<params>(?:\s+\S+)*)\s*$", re.I)
_ADDRESS = re.compile(r"^[^@\s]+@[^@\s]+$")
_MESSAGE_ID = re.compile(r"^message-id\s*:\s*(?P<id>.*)$", re.I)
_SUBJECT = re.compile(r"^subject\s*:\s*(?P<subject>.*)$", re.I)

#: Challenge prompts of AUTH LOGIN, RFC-4954-style base64 of "Username:" and "Password:".
_LOGIN_USER_PROMPT = base64.b64encode(b"Username:").decode("ascii")
_LOGIN_PASS_PROMPT = base64.b64encode(b"Password:").decode("ascii")


class _Closed(Exception):
    """The peer went away, or sent something no session can continue after."""


class SmtpSession:
    """One SMTP connection to one scope's listener."""

    def __init__(self, scope: "Scope", conn: socket.socket, peer: tuple) -> None:
        self.scope = scope
        self.conn = conn
        self.peer = peer
        self.rfile = conn.makefile("rb")
        self.tls = False
        self.greeted = False
        self.authed = False
        self.mail_from: str | None = None
        self.rcpts: list[str] = []
        self.cid = scope.new_correlation()

    # -- plumbing ---------------------------------------------------------------------------

    def run(self) -> None:
        try:
            if self.scope.protocol == "tcp" and self._probe_first():
                return
            self._reply(220, f"{CERT_HOSTNAME} ESMTP mail-mock ready", delayed=False)
            while self._step():
                pass
        except (OSError, ssl.SSLError, _Closed) as exc:
            log.info("smtp[%s] session ended: %s", self.scope.run_id, type(exc).__name__)
        finally:
            self._close()

    def _close(self) -> None:
        for closer in (self.rfile.close, self.conn.close):
            try:
                closer()
            except OSError:
                pass  # the peer or a scope close already tore the socket down

    def _log(self, direction: str, text: str) -> None:
        log.info("smtp[%s] %s %s", self.scope.run_id, direction, text)

    def _read_line(self) -> str:
        raw = self.rfile.readline(MAX_LINE_BYTES + 1)
        if not raw:
            raise _Closed("peer closed")
        if len(raw) > MAX_LINE_BYTES and not raw.endswith(b"\n"):
            self._reply(500, "5.5.2 line too long")
            raise _Closed("line too long")
        return raw.decode("utf-8", errors="replace").rstrip("\r\n")

    def _new_exchange(self) -> None:
        self.cid = self.scope.new_correlation()

    def _command_event(self, verb: str, line: str | None = None, *, secret: bool = False, fields: dict[str, str] | None = None) -> None:
        """Record one inbound command. A secret line contributes its verb only, never a digest of itself."""
        payload = clear_payload(fields) if fields else redacted_payload(verb if secret or line is None else line)
        self.scope.emit(
            "request.received", "inbound", f"SMTP {verb} received",
            protocol="smtp", correlation_id=self.cid, payload=payload,
        )

    def _reply(self, code: int, text: str | list[str], *, fault: str | None = None, delayed: bool = True) -> None:
        lines = text if isinstance(text, list) else [text]
        delay_ms = int(self.scope.get(manifest.SMTP_DELAY_MS)) if delayed else 0
        if delay_ms > 0:
            self.scope.emit(
                "fault.injected", "internal", f"{manifest.SMTP_DELAY_MS} held the reply {delay_ms} ms",
                protocol="smtp", correlation_id=self.cid, outcome="fault", fault=manifest.SMTP_DELAY_MS, latency_ms=delay_ms,
            )
            self.scope.closing.wait(delay_ms / 1000.0)
        outcome_fault = fault or (manifest.SMTP_DELAY_MS if delay_ms > 0 else None)
        self.scope.emit(
            "response.sent", "outbound", f"SMTP reply {code} {lines[0]}"[:200],
            protocol="smtp", correlation_id=self.cid, outcome="fault" if outcome_fault else "ok", fault=outcome_fault,
            latency_ms=delay_ms if delay_ms > 0 else None, payload=redacted_payload(lines[0]),
        )
        wire = "".join(f"{code}{'-' if i < len(lines) - 1 else ' '}{line}\r\n" for i, line in enumerate(lines))
        self.conn.sendall(wire.encode("utf-8"))
        self._log(">", f"{code} {lines[0]}")

    def _fault(self, setting: str, what: str) -> None:
        self.scope.emit(
            "fault.injected", "internal", f"{setting} {what}",
            protocol="smtp", correlation_id=self.cid, outcome="fault", fault=setting,
        )

    # -- the probe --------------------------------------------------------------------------

    def _probe_first(self) -> bool:
        """On a `tcp` listener, a client that speaks before the greeting is the conformance probe."""
        grace = self.scope.mock.probe_grace_seconds
        ready, _, _ = select.select([self.conn], [], [], grace)
        if not ready:
            return False
        if not self.conn.recv(1, socket.MSG_PEEK):
            return True  # the peer connected and closed without a word
        line = self._read_line()
        self._log("<", f"probe line, {len(line)} bytes")
        self.scope.emit(
            "request.received", "inbound", "probe line received",
            protocol="tcp", correlation_id=self.cid, payload=redacted_payload(line),
        )
        reply, fault = self._probe_answer()
        delay_ms = int(self.scope.get(manifest.SMTP_DELAY_MS))
        if fault:
            self.scope.emit(
                "fault.injected", "internal", f"{fault} changed the probe answer",
                protocol="tcp", correlation_id=self.cid, outcome="fault", fault=fault,
            )
        if delay_ms > 0:
            self.scope.closing.wait(delay_ms / 1000.0)
        self.scope.emit(
            "response.sent", "outbound", "probe answered",
            protocol="tcp", correlation_id=self.cid, outcome="fault" if fault else "ok", fault=fault,
            payload=redacted_payload(reply),
        )
        self.conn.sendall((reply + "\r\n").encode("utf-8"))
        return True

    def _probe_answer(self) -> tuple[str, str | None]:
        """The one line a probe is answered with, and the setting that shaped it."""
        if self.scope.get(manifest.TEMPFAIL):
            return manifest.PROBE_REPLY_TEMPFAIL, manifest.TEMPFAIL
        if self.scope.get(manifest.AUTH_REQUIRED):
            return manifest.PROBE_REPLY_AUTH_REQUIRED, manifest.AUTH_REQUIRED
        refused = str(self.scope.get(manifest.REFUSE_RECIPIENT))
        if refused.strip():
            return manifest.PROBE_REPLY_REFUSED_PREFIX + refused, manifest.REFUSE_RECIPIENT
        delay_ms = int(self.scope.get(manifest.SMTP_DELAY_MS))
        if delay_ms > 0:
            return manifest.probe_reply_delayed(delay_ms), manifest.SMTP_DELAY_MS
        return manifest.PROBE_REPLY_OK, None

    # -- the dialogue -----------------------------------------------------------------------

    def _step(self) -> bool:
        """Read and answer one command; False ends the session."""
        line = self._read_line()
        self._new_exchange()
        verb, _, rest = line.strip().partition(" ")
        verb = verb.upper()
        self._log("<", redaction_for_log(self.scope, redaction.KIND_COMMAND, line))
        handler = getattr(self, f"_cmd_{verb.lower()}", None) if verb.isalpha() else None
        if handler is None:
            self._command_event("UNKNOWN", verb)
            self._reply(500, "5.5.2 command not recognized")
            return True
        return handler(rest.strip()) is not False

    def _cmd_helo(self, arg: str) -> bool:
        return self._greet("HELO", arg, extended=False)

    def _cmd_ehlo(self, arg: str) -> bool:
        return self._greet("EHLO", arg, extended=True)

    def _greet(self, verb: str, arg: str, *, extended: bool) -> bool:
        self._command_event(verb, verb)
        if not arg:
            self._reply(501, f"5.5.4 {verb} needs the client's name")
            return True
        self._reset_transaction()
        self.greeted = True
        if not extended:
            self._reply(250, CERT_HOSTNAME)
            return True
        caps = [f"{CERT_HOSTNAME} at your service", f"SIZE {MAX_MESSAGE_BYTES}", "8BITMIME", "AUTH PLAIN LOGIN", "ENHANCEDSTATUSCODES"]
        if not self.tls:
            caps.insert(2, "STARTTLS")
        self._reply(250, caps)
        return True

    def _cmd_starttls(self, arg: str) -> bool:
        self._command_event("STARTTLS", "STARTTLS")
        if arg:
            self._reply(501, "5.5.4 STARTTLS takes no argument")
        elif self.tls:
            self._reply(503, "5.5.1 TLS is already active")
        else:
            self._reply(220, "2.0.0 ready to start TLS")
            self._upgrade()
        return True

    def _upgrade(self) -> None:
        self.rfile.close()
        try:
            self.conn = self.scope.certificate.context.wrap_socket(self.conn, server_side=True)
        except (ssl.SSLError, OSError) as exc:
            log.info("smtp[%s] tls handshake failed: %s", self.scope.run_id, type(exc).__name__)
            raise _Closed("tls handshake failed") from exc
        self.scope.track_smtp_connection(self.conn)
        self.rfile = self.conn.makefile("rb")
        self.tls = True
        self.greeted = False  # RFC 3207 §4.2: the client starts over with EHLO
        self.authed = False
        self._reset_transaction()

    def _cmd_auth(self, arg: str) -> bool:
        self._command_event("AUTH", secret=True)
        mech, _, initial = arg.partition(" ")
        mech = mech.upper()
        if not self.greeted:
            self._reply(503, "5.5.1 send EHLO first")
        elif self.authed:
            self._reply(503, "5.5.1 already authenticated")
        elif self.mail_from is not None:
            self._reply(503, "5.5.1 AUTH is not allowed during a mail transaction")
        elif mech == "PLAIN":
            self._auth_plain(initial.strip())
        elif mech == "LOGIN":
            self._auth_login(initial.strip())
        else:
            self._reply(504, "5.5.4 unrecognized authentication mechanism; use PLAIN or LOGIN")
        return True

    def _challenge(self, prompt: str) -> str | None:
        """Send a 334 challenge and read the secret answer; None when the client cancelled with '*'."""
        self._reply(334, prompt)
        answer = self._read_line()
        self._log("<", redaction_for_log(self.scope, redaction.KIND_AUTH_DATA, answer))
        return None if answer == "*" else answer

    def _auth_plain(self, initial: str) -> None:
        data = initial or self._challenge("")
        if data is None:
            self._reply(501, "5.0.0 authentication cancelled")
            return
        try:
            authzid, user, password = base64.b64decode(data, validate=True).decode("utf-8").split("\x00")
        except (binascii.Error, ValueError, UnicodeDecodeError):
            self._reply(501, "5.5.2 cannot decode the AUTH PLAIN response")
            return
        self._finish_auth(user, password)

    def _auth_login(self, initial: str) -> None:
        user_b64 = initial or self._challenge(_LOGIN_USER_PROMPT)
        if user_b64 is None:
            self._reply(501, "5.0.0 authentication cancelled")
            return
        pass_b64 = self._challenge(_LOGIN_PASS_PROMPT)
        if pass_b64 is None:
            self._reply(501, "5.0.0 authentication cancelled")
            return
        try:
            user = base64.b64decode(user_b64, validate=True).decode("utf-8")
            password = base64.b64decode(pass_b64, validate=True).decode("utf-8")
        except (binascii.Error, UnicodeDecodeError):
            self._reply(501, "5.5.2 cannot decode the AUTH LOGIN response")
            return
        self._finish_auth(user, password)

    def _finish_auth(self, user: str, password: str) -> None:
        if credentials_match(self.scope, user, password):
            self.authed = True
            self._reply(235, "2.7.0 authentication successful")
        else:
            self._reply(535, "5.7.8 authentication credentials invalid")

    def _cmd_mail(self, arg: str) -> bool:
        self._command_event("MAIL", "MAIL")
        match = _MAIL_FROM.match("MAIL " + arg)
        if not self.greeted:
            self._reply(503, "5.5.1 send EHLO or HELO first")
        elif self.scope.get(manifest.AUTH_REQUIRED) and not self.authed:
            self._fault(manifest.AUTH_REQUIRED, "answered MAIL FROM with 530")
            self._reply(530, "5.7.0 authentication required", fault=manifest.AUTH_REQUIRED)
        elif self.mail_from is not None:
            self._reply(503, "5.5.1 nested MAIL command")
        elif match is None:
            self._reply(501, "5.5.4 syntax: MAIL FROM:<address>")
        elif match.group("addr") and not _ADDRESS.match(match.group("addr")):
            self._reply(501, "5.1.7 the sender address is not valid")
        else:
            self._accept_mail_from(match.group("addr"), match.group("params"))
        return True

    def _accept_mail_from(self, addr: str, params: str) -> None:
        for param in params.split():
            key, _, value = param.partition("=")
            if key.upper() == "SIZE" and value.isdigit():
                if int(value) > MAX_MESSAGE_BYTES:
                    self._reply(552, "5.3.4 message size exceeds the limit")
                    return
            elif key.upper() != "BODY":
                self._reply(555, "5.5.4 parameter not recognized")
                return
        self.mail_from = addr
        self.scope.emit(
            "request.received", "inbound", "SMTP MAIL sender recorded",
            protocol="smtp", correlation_id=self.cid, payload=clear_payload({"from": addr}),
        )
        self._reply(250, "2.1.0 sender ok")

    def _cmd_rcpt(self, arg: str) -> bool:
        self._command_event("RCPT", "RCPT")
        match = _RCPT_TO.match("RCPT " + arg)
        if self.mail_from is None:
            self._reply(503, "5.5.1 need MAIL before RCPT")
        elif match is None:
            self._reply(501, "5.5.4 syntax: RCPT TO:<address>")
        elif not _ADDRESS.match(match.group("addr")):
            self._reply(501, "5.1.3 the recipient address is not valid")
        elif len(self.rcpts) >= MAX_RECIPIENTS:
            self._reply(452, "4.5.3 too many recipients")
        else:
            self._accept_rcpt(match.group("addr"))
        return True

    def _accept_rcpt(self, addr: str) -> None:
        self.scope.emit(
            "request.received", "inbound", "SMTP RCPT recipient recorded",
            protocol="smtp", correlation_id=self.cid, payload=clear_payload({"to": addr}),
        )
        if self.scope.get(manifest.TEMPFAIL):
            self._fault(manifest.TEMPFAIL, "answered RCPT TO with 451")
            self._reply(451, "4.3.0 temporary failure, try again later", fault=manifest.TEMPFAIL)
            return
        refused = {a.strip().lower() for a in str(self.scope.get(manifest.REFUSE_RECIPIENT)).split(",") if a.strip()}
        if addr.lower() in refused:
            self._fault(manifest.REFUSE_RECIPIENT, "answered RCPT TO with 550")
            self._reply(550, "5.1.1 recipient refused", fault=manifest.REFUSE_RECIPIENT)
            return
        self.rcpts.append(addr)
        self._reply(250, "2.1.5 recipient ok")

    def _cmd_data(self, arg: str) -> bool:
        self._command_event("DATA", "DATA")
        if arg:
            self._reply(501, "5.5.4 DATA takes no argument")
        elif self.mail_from is None:
            self._reply(503, "5.5.1 need MAIL before DATA")
        elif not self.rcpts:
            self._reply(503, "5.5.1 need an accepted RCPT before DATA")
        else:
            self._reply(354, "start mail input; end with <CRLF>.<CRLF>")
            self._receive_data()
        return True

    def _receive_data(self) -> None:
        body, too_big, too_long = bytearray(), False, False
        while True:
            raw = self.rfile.readline(MAX_DATA_LINE_BYTES + 1)
            if not raw:
                raise _Closed("peer closed during DATA")
            if len(raw) > MAX_DATA_LINE_BYTES:
                # Never split a line: the rest of it is body, so a piece that reads `.` must not end DATA.
                too_long = True
                while not raw.endswith(b"\n"):
                    raw = self.rfile.readline(MAX_DATA_LINE_BYTES)
                    if not raw:
                        raise _Closed("peer closed during DATA")
                continue
            if raw in (b".\r\n", b".\n"):
                break
            if raw.startswith(b"."):
                raw = raw[1:]  # RFC 5321 section 4.5.2: the receiver deletes the first dot of a line
            if not too_big and not too_long:
                body += raw.rstrip(b"\r\n") + b"\r\n"
                too_big = len(body) > MAX_MESSAGE_BYTES
        self._new_exchange()
        if too_long:
            self._reply(500, "5.5.6 a line of the message is longer than 1000 octets")
        elif too_big:
            self._reply(552, "5.3.4 message size exceeds the limit")
        else:
            self._store(bytes(body))
        self._reset_transaction()

    def _store(self, raw: bytes) -> None:
        raw, message_id = ensure_message_id(raw)
        subject = header_value(raw, _SUBJECT)
        for line in header_lines(raw):
            self._log("<", "header " + redaction_for_log(self.scope, redaction.KIND_HEADER, line))
        mail = StoredMail(raw=raw, mail_from=self.mail_from or "", recipients=tuple(self.rcpts), message_id=message_id)
        self.scope.mailbox.add(mail)
        self.cid = self.scope.correlation_for_message(message_id)
        self.scope.emit(
            "request.received", "inbound", "SMTP message content received",
            protocol="smtp", correlation_id=self.cid, correlation_source="correlation-key",
            correlation_keys={"message_id": message_id}, payload=redacted_payload(raw),
        )
        self.scope.emit(
            "message.delivered", "outbound", "mail stored in the scope's mailbox",
            protocol="smtp", correlation_id=self.cid, correlation_source="correlation-key",
            correlation_keys={"message_id": message_id}, outcome="ok",
            payload=clear_payload({
                "from": mail.mail_from, "to": ", ".join(mail.recipients), "subject": subject, "message_id": message_id,
            }),
        )
        self._reply(250, f"2.0.0 ok, queued as {uuid.uuid4().hex[:12]}")

    def _reset_transaction(self) -> None:
        self.mail_from = None
        self.rcpts = []

    def _cmd_rset(self, arg: str) -> bool:
        self._command_event("RSET", "RSET")
        self._reset_transaction()
        self._reply(250, "2.0.0 reset")
        return True

    def _cmd_noop(self, arg: str) -> bool:
        self._command_event("NOOP", "NOOP")
        self._reply(250, "2.0.0 ok")
        return True

    def _cmd_quit(self, arg: str) -> bool:
        self._command_event("QUIT", "QUIT")
        self._reply(221, "2.0.0 bye")
        return False

    def _cmd_vrfy(self, arg: str) -> bool:
        self._command_event("VRFY", "VRFY")
        self._reply(502, "5.5.1 VRFY is not implemented")
        return True

    _cmd_expn = _cmd_vrfy


# -- helpers shared with the tests -----------------------------------------------------------


def credentials_match(scope: "Scope", user: str, password: str) -> bool:
    """Compare against the scope's credentials without short-circuiting between user and password."""
    want_user = str(scope.get(manifest.AUTH_USER)).encode("utf-8")
    want_pass = str(scope.get(manifest.AUTH_PASSWORD)).encode("utf-8")
    user_ok = hmac.compare_digest(user.encode("utf-8"), want_user)
    pass_ok = hmac.compare_digest(password.encode("utf-8"), want_pass)
    return user_ok and pass_ok


def redaction_for_log(scope: "Scope", kind: str, line: str) -> str:
    """What the scope's redactor lets into the log for `line`."""
    return scope.mock.redactor(kind, line)


def header_lines(raw: bytes) -> list[str]:
    """The unfolded-start header lines of a message, up to the blank line."""
    out: list[str] = []
    for line in raw.decode("utf-8", errors="replace").split("\r\n"):
        if not line:
            break
        if line[0] not in " \t":
            out.append(line)
    return out


def header_value(raw: bytes, pattern: re.Pattern[str]) -> str:
    """The value of the first header line matching `pattern` (its first group), or ''."""
    for line in header_lines(raw):
        m = pattern.match(line)
        if m:
            return m.group(1).strip()
    return ""


def ensure_message_id(raw: bytes) -> tuple[bytes, str]:
    """The mail's Message-ID; a mail without one gets one, as a relay does."""
    existing = header_value(raw, _MESSAGE_ID)
    if existing:
        return raw, existing
    generated = f"<{uuid.uuid4().hex}@{CERT_HOSTNAME}>"
    return f"Message-ID: {generated}\r\n".encode("ascii") + raw, generated
