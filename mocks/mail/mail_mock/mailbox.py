"""One scope's mailbox: the mail its SMTP side accepted, read back by its IMAP side."""

from __future__ import annotations

import threading
from dataclasses import dataclass, field
from email import message_from_bytes, policy
from email.message import EmailMessage


@dataclass(frozen=True)
class StoredMail:
    """One accepted mail, immutable once stored."""

    raw: bytes  # the message as received, CRLF line ends, dot-unstuffed
    mail_from: str
    recipients: tuple[str, ...]
    message_id: str

    def parsed(self) -> EmailMessage:
        msg = message_from_bytes(self.raw, policy=policy.SMTP)
        assert isinstance(msg, EmailMessage)
        return msg


@dataclass
class Mailbox:
    """The INBOX of one scope; two scopes never share one."""

    _mails: list[StoredMail] = field(default_factory=list)
    _lock: threading.Lock = field(default_factory=threading.Lock)

    def add(self, mail: StoredMail) -> int:
        """Store a mail and return its message number (1-based)."""
        with self._lock:
            self._mails.append(mail)
            return len(self._mails)

    def snapshot(self) -> list[StoredMail]:
        with self._lock:
            return list(self._mails)

    def __len__(self) -> int:
        with self._lock:
            return len(self._mails)
