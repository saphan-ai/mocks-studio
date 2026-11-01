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
_IMAP_TOKEN = re.compile(r'"(?:[^"\\]|\\.)*"|\(|\)|[^\s()"]+')
_IMAP_CREDENTIAL_VERBS = frozenset({"LOGIN", "AUTHENTICATE"})
_LITERAL_MARKER = re.compile(r"\{\d+\+?\}$")
#: The only word after SMTP `AUTH` that is logged; any other word may be the credential itself.
_AUTH_MECHANISMS = frozenset({"PLAIN", "LOGIN"})

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


def imap_tokens(line: str) -> list[str]:
    """Split an IMAP command line into atoms, quoted strings (unquoted here) and parentheses."""
    out = []
    for tok in _IMAP_TOKEN.findall(line):
        if tok.startswith('"'):
            tok = re.sub(r"\\(.)", r"\1", tok[1:-1])
        out.append(tok)
    return out


def _imap_credential_command(line: str) -> tuple[str, str] | None:
    """The tag and verb of an IMAP LOGIN or AUTHENTICATE, read the way the session reads it (leading space, quoted verb)."""
    tokens = imap_tokens(line)
    if len(tokens) >= 2 and tokens[1].upper() in _IMAP_CREDENTIAL_VERBS:
        return tokens[0], tokens[1].upper()
    return None


def starts_literal_credential(line: str) -> bool:
    """Whether an IMAP LOGIN or AUTHENTICATE line ends in a literal marker, so the NEXT line is the secret."""
    return _imap_credential_command(line) is not None and _LITERAL_MARKER.search(line.rstrip("\r\n")) is not None


def _redact_command(line: str) -> str:
    verb = _VERB.match(line)
    if verb and verb.group("verb").upper() == "AUTH":  # SMTP first: "AUTH LOGIN" must not read as an IMAP tag and verb
        parts = line.split()
        mech = parts[1].upper() if len(parts) > 1 and parts[1].upper() in _AUTH_MECHANISMS else ""
        return f"AUTH {mech} {MARKER}".replace("  ", " ")
    credential = _imap_credential_command(line)
    if credential:
        return f"{credential[0]} {credential[1]} {MARKER}"
    return line[:MAX_LOGGED_LINE]
