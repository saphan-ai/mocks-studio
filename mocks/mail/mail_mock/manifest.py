"""The mock's manifest (`mocks.adapter/v1`) and the declared settings.

The manifest is built here, in one place; `mocks.manifest.json` beside the package is its
rendering (`python -m mail_mock manifest`), and a test keeps the two equal.
"""

from __future__ import annotations

import copy
from typing import Any

CONTRACT = "mocks.adapter/v1"
NAME = "mail-mock"
VERSION = "0.1.0"

#: Where the manifest file says the control endpoint is; the served manifest carries the real one.
DEFAULT_CONTROL_URL = "http://127.0.0.1:19100"

#: The protocols a scope's listener may speak. `smtp` is a plain SMTP listener; `tcp` is the same
#: SMTP listener that also answers the conformance probe line (see `probe.py`).
PROTOCOLS = ["smtp", "tcp"]

#: How long a `tcp` listener waits for a client to speak before it sends the SMTP greeting, in
#: milliseconds. A client that speaks first is the conformance probe; a real SMTP client waits.
#: It lives here, not in `app`, because the command line needs it before `cryptography` is imported.
DEFAULT_PROBE_GRACE_MS = 150

SAFE_FIELDS =["from", "to", "subject", "message_id"]
CORRELATION_KEYS = ["message_id"]

#: Fictional credentials a scope starts with, for SMTP AUTH and IMAP LOGIN alike.
DEFAULT_USER = "agent@acme-example.test"
DEFAULT_PASSWORD = "fictional-password-for-tests"

#: Names of the settings, so code refers to them by constant rather than by literal.
REFUSE_RECIPIENT = "refuse_recipient"
AUTH_REQUIRED = "auth_required"
SMTP_DELAY_MS = "smtp_delay_ms"
TEMPFAIL = "tempfail"
AUTH_USER = "auth_user"
AUTH_PASSWORD = "auth_password"

#: Bounds of `smtp_delay_ms`.
DELAY_MIN_MS = 0
DELAY_MAX_MS = 10000

#: The probe reply lines, one per setting, as `effect.expect.tcp_reply` states them.
PROBE_REPLY_OK = "250 2.0.0 ok"
PROBE_REPLY_TEMPFAIL = "451 4.3.0 temporary failure, try again later"
PROBE_REPLY_AUTH_REQUIRED = "530 5.7.0 authentication required"
PROBE_REPLY_REFUSED_PREFIX = "550 5.1.1 recipient refused: "
PROBE_REPLY_DELAY_PREFIX = "250 2.0.0 ok, replies delayed "

_EFFECT_ADDRESS = "refused@acme-example.test"
_EFFECT_DELAY_MS = 250


def probe_reply_delayed(delay_ms: int) -> str:
    """The probe reply while `smtp_delay_ms` is `delay_ms`."""
    return f"{PROBE_REPLY_DELAY_PREFIX}{delay_ms} ms"


def settings() -> list[dict[str, Any]]:
    """The declared settings, each with the effect the suite observes on a `tcp` listener."""
    return [
        {
            "name": REFUSE_RECIPIENT,
            "description": "Comma-separated recipient addresses answered 550 at RCPT TO; empty refuses none.",
            "type": "string",
            "default": "",
            "scope": "run",
            "effect": {
                "protocol": "tcp",
                "value": _EFFECT_ADDRESS,
                "expect": {"tcp_reply": PROBE_REPLY_REFUSED_PREFIX + _EFFECT_ADDRESS},
            },
        },
        {
            "name": AUTH_REQUIRED,
            "description": "When true, MAIL FROM before a successful AUTH is answered 530.",
            "type": "boolean",
            "default": False,
            "scope": "run",
            "effect": {"protocol": "tcp", "value": True, "expect": {"tcp_reply": PROBE_REPLY_AUTH_REQUIRED}},
        },
        {
            "name": SMTP_DELAY_MS,
            "description": "Milliseconds every SMTP reply after the greeting is held back.",
            "type": "integer",
            "default": 0,
            "minimum": DELAY_MIN_MS,
            "maximum": DELAY_MAX_MS,
            "scope": "run",
            "effect": {
                "protocol": "tcp",
                "value": _EFFECT_DELAY_MS,
                "expect": {"tcp_reply": probe_reply_delayed(_EFFECT_DELAY_MS)},
            },
        },
        {
            "name": TEMPFAIL,
            "description": "When true, every RCPT TO is answered 451.",
            "type": "boolean",
            "default": False,
            "scope": "run",
            "effect": {"protocol": "tcp", "value": True, "expect": {"tcp_reply": PROBE_REPLY_TEMPFAIL}},
        },
        {
            "name": AUTH_USER,
            "description": "The user name SMTP AUTH and IMAP LOGIN accept (fictional default).",
            "type": "string",
            "default": DEFAULT_USER,
            "scope": "run",
        },
        {
            "name": AUTH_PASSWORD,
            "description": "The password SMTP AUTH and IMAP LOGIN accept (fictional default); never logged, never in an event.",
            "type": "string",
            "default": DEFAULT_PASSWORD,
            "scope": "run",
        },
    ]


def build(control_url: str = DEFAULT_CONTROL_URL) -> dict[str, Any]:
    """The manifest, with `control.url` set to where the control endpoint really is."""
    return {
        "contract": CONTRACT,
        "name": NAME,
        "version": VERSION,
        "description": "SMTP relay with STARTTLS and AUTH, and an IMAP mailbox where caught mail is read back by Message-ID.",
        "protocols": list(PROTOCOLS),
        "control": {"url": control_url},
        "settings": copy.deepcopy(settings()),
        "safe_fields": list(SAFE_FIELDS),
        "correlation_keys": list(CORRELATION_KEYS),
        "conformance": {"probe": {"tcp": {"send": "ping"}}},
    }
