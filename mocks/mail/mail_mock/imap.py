"""The IMAP side: the IMAP4rev1 subset (RFC 3501) that reads a caught mail back from INBOX.

LOGIN, CAPABILITY, NOOP, SELECT/EXAMINE INBOX, SEARCH (ALL, HEADER), FETCH (RFC822, BODY[],
ENVELOPE, plus FLAGS, UID, RFC822.SIZE) and LOGOUT. Anything else is answered BAD.
"""

from __future__ import annotations

import logging
import re
import socket
import ssl
from email.utils import getaddresses
from typing import TYPE_CHECKING

from . import redact as redaction
from .events import redacted_payload
from .mailbox import StoredMail
from .smtp import credentials_match, redaction_for_log

if TYPE_CHECKING:
    from .scope import Scope

log = logging.getLogger("mail_mock")

MAX_LINE_BYTES = 8192
_SEQ_ITEM = re.compile(r"^(\d+|\*)(?::(\d+|\*))?$")

_FETCH_ITEMS = {"RFC822", "BODY[]", "BODY.PEEK[]", "ENVELOPE", "FLAGS", "UID", "RFC822.SIZE"}


class _Closed(Exception):
    """The peer went away or logged out."""


class _Bad(Exception):
    """A command the session answers BAD (or NO, by `status`)."""

    def __init__(self, text: str, status: str = "BAD") -> None:
        super().__init__(text)
        self.text = text
        self.status = status


def tokenize(line: str) -> list[str]:
    """Split an IMAP command line into atoms, quoted strings (unquoted here) and parentheses."""
    return redaction.imap_tokens(line)  # one tokenizer for the session and the redactor, so they never disagree


def nstring(value: str | None) -> bytes:
    """An IMAP nstring: NIL, a quoted string, or a literal when the text needs one."""
    if value is None or value == "":
        return b"NIL"
    data = value.encode("utf-8", errors="surrogateescape")  # a raw 8-bit header comes back as the bytes it arrived as
    if any(b < 32 or b > 126 for b in data):
        return b"{%d}\r\n" % len(data) + data
    return b'"' + data.replace(b"\\", b"\\\\").replace(b'"', b'\\"') + b'"'


def _address_list(values: list[str]) -> bytes:
    pairs = [(n, a) for n, a in getaddresses(values) if a]
    if not pairs:
        return b"NIL"
    out = b"("
    for name, addr in pairs:
        mailbox, _, host = addr.partition("@")
        out += b"(" + nstring(name) + b" NIL " + nstring(mailbox) + b" " + nstring(host or None) + b")"
    return out + b")"


def envelope(mail: StoredMail) -> bytes:
    """The ENVELOPE structure of RFC 3501 §7.4.2 for a stored mail."""
    msg = mail.parsed()

    # The header values as received: RFC 3501 section 7.4.2 sends RFC 2047 encoded words undecoded.
    raw_items = [(k.lower(), re.sub(r"\r?\n(?=[ \t])", "", v).strip()) for k, v in msg.raw_items()]

    def addrs(name: str) -> list[str]:
        return [v for k, v in raw_items if k == name.lower()]

    def hdr(name: str) -> str | None:
        found = addrs(name)
        return found[0] if found else None

    sender = addrs("Sender") or addrs("From")
    reply_to = addrs("Reply-To") or addrs("From")
    parts = [
        nstring(hdr("Date")), nstring(hdr("Subject")), _address_list(addrs("From")), _address_list(sender),
        _address_list(reply_to), _address_list(addrs("To")), _address_list(addrs("Cc")), _address_list(addrs("Bcc")),
        nstring(hdr("In-Reply-To")), nstring(mail.message_id),
    ]
    return b"(" + b" ".join(parts) + b")"


def parse_sequence_set(text: str, count: int) -> list[int]:
    """Message numbers named by a sequence set such as `1`, `2:4`, `1:*` or `1,3`."""
    numbers: list[int] = []
    for part in text.split(","):
        m = _SEQ_ITEM.match(part)
        if not m:
            raise _Bad("the message set is not valid")
        lo = count if m.group(1) == "*" else int(m.group(1))
        hi = lo if m.group(2) is None else (count if m.group(2) == "*" else int(m.group(2)))
        lo, hi = min(lo, hi), max(lo, hi)
        numbers.extend(range(lo, hi + 1))
    if any(n < 1 or n > count for n in numbers):
        raise _Bad("message number out of range", "NO")
    return numbers


class ImapSession:
    """One IMAP connection to one scope's mailbox."""

    def __init__(self, scope: "Scope", conn: socket.socket, peer: tuple) -> None:
        self.scope = scope
        self.conn = conn
        self.rfile = conn.makefile("rb")
        self.authed = False
        self.selected = False
        self.secret_follows = False  # the last line was a LOGIN or AUTHENTICATE ending in a literal: the next line is the secret

    def run(self) -> None:
        try:
            self._send(b"* OK IMAP4rev1 mail-mock ready\r\n", "greeting")
            while True:
                self._step()
        except (OSError, ssl.SSLError, _Closed) as exc:
            log.info("imap[%s] session ended: %s", self.scope.run_id, type(exc).__name__)
        finally:
            for closer in (self.rfile.close, self.conn.close):
                try:
                    closer()
                except OSError:
                    pass  # already torn down by the peer or a scope close

    def _send(self, data: bytes, summary: str) -> None:
        self.scope.emit(
            "response.sent", "outbound", f"IMAP {summary}"[:200],
            protocol="tcp", payload=redacted_payload(summary),
        )
        self.conn.sendall(data)

    def _step(self) -> None:
        raw = self.rfile.readline(MAX_LINE_BYTES + 1)
        if not raw:
            raise _Closed("peer closed")
        line = raw.decode("utf-8", errors="replace").rstrip("\r\n")
        kind = redaction.KIND_AUTH_DATA if self.secret_follows else redaction.KIND_COMMAND
        self.secret_follows = redaction.starts_literal_credential(line)
        log.info("imap[%s] < %s", self.scope.run_id, redaction_for_log(self.scope, kind, line))
        tokens = tokenize(line)
        if len(tokens) < 2:
            self._untagged_bad("a command is a tag and a verb")
            return
        tag, verb = tokens[0], tokens[1].upper()
        self.scope.emit(
            "request.received", "inbound", f"IMAP {verb} received" if verb.isalpha() else "IMAP command received",
            protocol="tcp", payload=redacted_payload(verb),  # the arguments may be a password
        )
        try:
            handler = getattr(self, f"_cmd_{verb.lower()}", None) if verb.isalpha() else None
            if handler is None:
                raise _Bad("command not recognized")
            body, text = handler(tokens[2:])
            self._send(body + f"{tag} OK {text}\r\n".encode("utf-8"), f"{verb} OK")
        except _Bad as bad:
            self._send(f"{tag} {bad.status} {bad.text}\r\n".encode("utf-8"), f"{verb} {bad.status}")
        if verb == "LOGOUT":
            raise _Closed("logout")

    def _untagged_bad(self, text: str) -> None:
        self._send(f"* BAD {text}\r\n".encode("utf-8"), "BAD")

    # -- commands: each returns (untagged output, completion text) --------------------------

    def _cmd_capability(self, args: list[str]) -> tuple[bytes, str]:
        return b"* CAPABILITY IMAP4rev1\r\n", "CAPABILITY completed"

    def _cmd_noop(self, args: list[str]) -> tuple[bytes, str]:
        body = b"* %d EXISTS\r\n" % len(self.scope.mailbox) if self.selected else b""
        return body, "NOOP completed"

    def _cmd_logout(self, args: list[str]) -> tuple[bytes, str]:
        return b"* BYE logging out\r\n", "LOGOUT completed"

    def _cmd_login(self, args: list[str]) -> tuple[bytes, str]:
        if len(args) != 2:
            raise _Bad("syntax: LOGIN user password")
        if self.authed:
            raise _Bad("already logged in")
        if not credentials_match(self.scope, args[0], args[1]):
            raise _Bad("[AUTHENTICATIONFAILED] invalid credentials", "NO")
        self.authed = True
        return b"", "LOGIN completed"

    def _cmd_select(self, args: list[str]) -> tuple[bytes, str]:
        if not self.authed:
            raise _Bad("log in first")
        if len(args) != 1:
            raise _Bad("syntax: SELECT mailbox")
        if args[0].upper() != "INBOX":
            raise _Bad("[NONEXISTENT] only INBOX exists", "NO")
        self.selected = True
        count = len(self.scope.mailbox)
        body = (
            b"* FLAGS (\\Seen)\r\n* %d EXISTS\r\n* 0 RECENT\r\n* OK [UIDVALIDITY 1] uids are message numbers\r\n" % count
        )
        return body, "[READ-WRITE] SELECT completed"

    _cmd_examine = _cmd_select

    def _require_selected(self) -> list[StoredMail]:
        if not self.selected:
            raise _Bad("select INBOX first")
        return self.scope.mailbox.snapshot()

    def _cmd_search(self, args: list[str]) -> tuple[bytes, str]:
        mails = self._require_selected()
        wanted = self._criteria(args)  # judged before any mail, so an empty INBOX still refuses a bad search
        hits = [n for n, mail in enumerate(mails, start=1) if self._matches(mail, wanted)]
        return b"* SEARCH" + b"".join(b" %d" % n for n in hits) + b"\r\n", "SEARCH completed"

    @staticmethod
    def _criteria(args: list[str]) -> list[tuple[str, str]]:
        """The (header name, needle) pairs of a search; ALL contributes none."""
        wanted: list[tuple[str, str]] = []
        i = 0
        while i < len(args):
            crit = args[i].upper()
            if crit == "ALL":
                i += 1
            elif crit == "HEADER" and i + 2 < len(args):
                wanted.append((args[i + 1], args[i + 2].lower()))
                i += 3
            else:
                raise _Bad("unsupported SEARCH criterion; use ALL or HEADER name value")
        return wanted

    @staticmethod
    def _matches(mail: StoredMail, wanted: list[tuple[str, str]]) -> bool:
        """Whether a mail has, for every pair, a header of that name containing the needle."""
        msg = mail.parsed()
        return all(any(needle in str(v).lower() for v in (msg.get_all(name) or [])) for name, needle in wanted)

    def _cmd_fetch(self, args: list[str]) -> tuple[bytes, str]:
        mails = self._require_selected()
        if len(args) < 2:
            raise _Bad("syntax: FETCH set items")
        numbers = parse_sequence_set(args[0], len(mails))
        items = [t.upper() for t in args[1:] if t not in ("(", ")")]
        unsupported = [t for t in items if t not in _FETCH_ITEMS]
        if unsupported:
            raise _Bad("unsupported FETCH item; use RFC822, BODY[], ENVELOPE, FLAGS, UID or RFC822.SIZE")
        out = b""
        for n in numbers:
            parts = [self._item(item, n, mails[n - 1]) for item in items]
            out += b"* %d FETCH (" % n + b" ".join(parts) + b")\r\n"
        return out, "FETCH completed"

    @staticmethod
    def _item(item: str, number: int, mail: StoredMail) -> bytes:
        if item in ("RFC822", "BODY[]", "BODY.PEEK[]"):
            label = b"RFC822" if item == "RFC822" else b"BODY[]"
            return label + b" {%d}\r\n" % len(mail.raw) + mail.raw
        if item == "ENVELOPE":
            return b"ENVELOPE " + envelope(mail)
        if item == "UID":
            return b"UID %d" % number
        if item == "RFC822.SIZE":
            return b"RFC822.SIZE %d" % len(mail.raw)
        return b"FLAGS ()"
