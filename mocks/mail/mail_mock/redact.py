"""What the mock may write to a log: never a credential, never a secret-named header's value.

Every line that reaches a log passes through `redact(kind, line)`. The redactor is injectable
(`MailMock(redactor=...)`) so a test can prove the no-secret test fails against a logger that
prints the raw line.
"""

from __future__ import annotations

import re
from collections.abc import Callable

#: What stands where a secret was.
MARKER = "[REDACTED]"

#: The kinds of line a redactor is asked about.
KIND_COMMAND = "command"  # a protocol command line, SMTP or IMAP
KIND_AUTH_DATA = "auth-data"  # a line sent in answer to an AUTH challenge: it is the secret itself
KIND_HEADER = "header"  # one header line of a received mail

Redactor = Callable[[str, str], str]

#: Short words that mark a header only as a whole `-`/`_`/`.`-separated part (`pin` is inside `shipping`).
_SECRET_WORDS = frozenset({"pin", "pwd", "otp"})
#: Names that mark a header wherever they occur once case and the separators are gone, so `X-Api-Key`,
#: `X_API_KEY` and `apikey` are one name. Written with their separators for the reader, normalised on use.
_SECRET_NAMES = (
    "password", "passwd", "token", "secret", "authorization", "credential",
    "api-key", "auth-key", "cookie", "set-cookie",
)
_NAME_SPLIT = re.compile(r"[-_.]")


def _normalised(name: str) -> str:
    return _NAME_SPLIT.sub("", name.lower())


_SECRET_JOINED = tuple(_normalised(name) for name in _SECRET_NAMES)

_HEADER_NAME = re.compile(r"^(?P<name>[^:\s]+)\s*:")
_VERB = re.compile(r"^\s*(?P<verb>\S+)")
_IMAP_TAGGED = re.compile(r"^(?P<tag>\S+)\s+(?P<verb>\S+)", re.ASCII)

#: Maximum length of a non-secret line that is logged.
MAX_LOGGED_LINE = 200


def is_secret_header(name: str) -> bool:
    """Whether a header called `name` is named like a password, a PIN or a token."""
    if any(joined in _normalised(name) for joined in _SECRET_JOINED):
        return True
    return any(part in _SECRET_WORDS for part in _NAME_SPLIT.split(name.lower()))


def redact(kind: str, line: str) -> str:
    """The form of `line` that may be logged."""
    line = line.rstrip("\r\n")
    if kind == KIND_AUTH_DATA:
        return MARKER
    if kind == KIND_HEADER:
        m = _HEADER_NAME.match(line)
        if m and is_secret_header(m.group("name")):
            return f"{m.group('name')}: {MARKER}"
        return line[:MAX_LOGGED_LINE]
    return _redact_command(line)


def _redact_command(line: str) -> str:
    verb = _VERB.match(line)
    if verb and verb.group("verb").upper() == "AUTH":  # SMTP first: "AUTH LOGIN" must not read as an IMAP tag and verb
        parts = line.split()
        mech = parts[1].upper() if len(parts) > 1 else ""
        return f"AUTH {mech} {MARKER}".replace("  ", " ")
    tagged = _IMAP_TAGGED.match(line)
    if tagged and tagged.group("verb").upper() in {"LOGIN", "AUTHENTICATE"}:
        return f"{tagged.group('tag')} {tagged.group('verb').upper()} {MARKER}"
    return line[:MAX_LOGGED_LINE]
