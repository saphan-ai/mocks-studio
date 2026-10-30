"""No secret in a log or an event: a password, a PIN, a token, or any base64 form of them.

The instrument (`leaks`) is itself checked: it must find a planted secret, and the same scenario
run against a logger that prints the raw line must fail it (law #27, negative control).
"""

from __future__ import annotations

import base64
import contextlib
import hashlib
import imaplib
import io
import smtplib
from unittest import mock as umock

from mail_mock import manifest, redact
from mail_mock.app import MailMock

from .support import HOST, TEST_PROBE_GRACE_MS, WAIT_SECONDS, EventTap, LogCapture, MockTestCase

PASSWORD = "Zq7-fictional-Pa55phrase"
WRONG_PASSWORD = "Wr0ng-fictional-Pa55phrase"
PIN = "pin-7482-9136"
TOKEN = "tok-f3a9c1d27b44"
API_KEY = "key-9c41e7d0a2b8"
COOKIE = "ck-51d0e8a3f7c6"
USER = manifest.DEFAULT_USER


def b64(text: str) -> str:
    return base64.b64encode(text.encode("utf-8")).decode("ascii")


#: Every form of a secret a log or an event must not carry. Padding is stripped, so a padded form is covered too.
SECRETS = {
    "password": PASSWORD,
    "wrong password": WRONG_PASSWORD,
    "pin": PIN,
    "token": TOKEN,
    "api key": API_KEY,
    "cookie": COOKIE,
    "base64 of the password": b64(PASSWORD).rstrip("="),
    "base64 of the wrong password": b64(WRONG_PASSWORD).rstrip("="),
    "base64 of the pin": b64(PIN).rstrip("="),
    "base64 of the token": b64(TOKEN).rstrip("="),
    "AUTH PLAIN string": b64(f"\0{USER}\0{PASSWORD}").rstrip("="),
    "AUTH PLAIN string, wrong password": b64(f"\0{USER}\0{WRONG_PASSWORD}").rstrip("="),
}


def digest_forms(value: str) -> list[str]:
    """The SHA-256 of a secret, as hex and as base64 (padding stripped): a digest is an encoding too (spec section 6)."""
    digest = hashlib.sha256(value.encode("utf-8")).digest()
    return [digest.hex(), base64.b64encode(digest).decode("ascii").rstrip("="), base64.urlsafe_b64encode(digest).decode("ascii").rstrip("=")]


#: The digest of every secret and every secret-bearing line the scenario sends.
SECRET_DIGESTS = {
    f"sha256 of {name} ({i})": form
    for name, value in SECRETS.items()
    for i, form in enumerate(digest_forms(value))
}
SECRET_DIGESTS.update({f"sha256 of the AUTH PLAIN line ({i})": f for i, f in enumerate(digest_forms(f"AUTH PLAIN {b64(chr(0) + USER + chr(0) + PASSWORD)}"))})


def leaks(captured: str) -> list[str]:
    """The names of the secrets, and of their SHA-256 digests, that appear in `captured`."""
    return [name for name, value in {**SECRETS, **SECRET_DIGESTS}.items() if value in captured]


class Scenario:
    """Authenticates over SMTP (PLAIN, LOGIN, one wrong) and IMAP with known secrets and a PIN-bearing mail."""

    def __init__(self, test: MockTestCase, mock: MailMock) -> None:
        self.test, self.mock = test, mock

    def run(self) -> tuple[str, str]:
        """Returns (everything logged, printed and written to stderr), (every event)."""
        tap = EventTap(self.test, self.mock)
        logs = LogCapture()
        out, err = io.StringIO(), io.StringIO()
        with logs, contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            scope = self.mock.open_scope({"run_id": "secret-run", "listener": {"protocol": "smtp", "host": HOST, "port": 0}})
            self.mock.put_settings("secret-run", {manifest.AUTH_PASSWORD: PASSWORD})
            self._smtp(scope)
            self._imap(scope)
            self.mock.close_scope("secret-run")
        return logs.text() + out.getvalue() + err.getvalue(), tap.raw_text()

    def _smtp(self, scope) -> None:
        msg = self.test.message(subject="Your code", body=f"Your PIN is {PIN}\n")
        msg["X-Pin"] = PIN
        msg["X-Api-Token"] = TOKEN
        msg["X-Api-Key"] = API_KEY
        msg["Cookie"] = f"session={COOKIE}"
        with smtplib.SMTP(HOST, scope.port, timeout=WAIT_SECONDS) as smtp:
            smtp.ehlo()
            smtp.starttls(context=self.test.trust(scope))
            smtp.ehlo()
            smtp.login(USER, PASSWORD)  # AUTH PLAIN
            smtp.send_message(msg)
        with smtplib.SMTP(HOST, scope.port, timeout=WAIT_SECONDS) as smtp:
            smtp.ehlo()
            smtp.user, smtp.password = USER, PASSWORD
            smtp.auth("LOGIN", smtp.auth_login)
        with smtplib.SMTP(HOST, scope.port, timeout=WAIT_SECONDS) as smtp:
            smtp.ehlo()
            try:
                smtp.login(USER, WRONG_PASSWORD)
            except smtplib.SMTPAuthenticationError:
                pass  # the refusal is the point

    def _imap(self, scope) -> None:
        client = imaplib.IMAP4(HOST, scope.imap_port, timeout=WAIT_SECONDS)
        try:
            client.login(USER, WRONG_PASSWORD)
        except imaplib.IMAP4.error:
            pass  # the refusal is the point
        client.login(USER, PASSWORD)
        client.select("INBOX")
        client.search(None, "ALL")
        client.fetch("1", "(RFC822)")
        client.logout()


class NoSecretTest(MockTestCase):
    def new_mock(self, **kw) -> MailMock:
        mock = MailMock(probe_grace_ms=TEST_PROBE_GRACE_MS, **kw)
        self.addCleanup(mock.shutdown)
        return mock

    def test_leaks_finds_a_planted_secret_in_each_form(self) -> None:
        for name, value in SECRETS.items():
            with self.subTest(form=name):
                self.assertEqual(leaks(f"something {value} something"), [name])

    def test_leaks_finds_a_planted_digest_of_a_secret_in_hex_and_in_base64(self) -> None:
        for form in digest_forms(PASSWORD):
            with self.subTest(form=form[:8]):
                self.assertNotEqual(leaks(f"digest={form}"), [])
        self.assertEqual(leaks("digest=" + hashlib.sha256(b"not a secret").hexdigest()), [])

    def test_no_password_pin_token_or_base64_of_them_reaches_a_log_or_an_event(self) -> None:
        logged, events = Scenario(self, self.new_mock()).run()
        self.assertEqual(leaks(logged), [], "a secret reached the log")
        self.assertEqual(leaks(events), [], "a secret reached an event")

    def test_the_scenario_really_exercised_the_redaction(self) -> None:
        logged, events = Scenario(self, self.new_mock()).run()
        for expected in ("AUTH PLAIN [REDACTED]", "AUTH LOGIN [REDACTED]", "LOGIN [REDACTED]", "X-Pin: [REDACTED]", "X-Api-Token: [REDACTED]", "X-Api-Key: [REDACTED]", "Cookie: [REDACTED]"):
            self.assertIn(expected, logged)
        self.assertIn("message.delivered", events, "the PIN-bearing mail must have been delivered")

    def test_a_logger_that_prints_the_raw_line_fails_the_no_secret_test(self) -> None:
        raw_logger = self.new_mock(redactor=lambda kind, line: line)
        logged, events = Scenario(self, raw_logger).run()
        found = leaks(logged)
        self.assertIn("pin", found)
        self.assertIn("token", found)
        self.assertIn("api key", found)
        self.assertIn("cookie", found)
        self.assertIn("AUTH PLAIN string", found)
        self.assertEqual(leaks(events), [], "events never carry a secret, whatever the logger does")

    def test_replacing_the_default_redactor_with_the_raw_value_fails_the_no_secret_test(self) -> None:
        with umock.patch.object(redact, "redact", lambda kind, line: line):
            logged, _ = Scenario(self, self.new_mock()).run()
        self.assertNotEqual(leaks(logged), [])


class RedactionUnitTest(MockTestCase):
    def test_header_lines_named_like_a_secret_keep_their_name_and_lose_their_value(self) -> None:
        for name in ("X-Pin", "Password", "X-Reset-Token", "X-Api-Token", "Authorization", "X_PIN", "X-Client-Secret"):
            with self.subTest(header=name):
                self.assertEqual(redact.redact(redact.KIND_HEADER, f"{name}: hunter2"), f"{name}: [REDACTED]")

    def test_api_key_auth_key_and_cookie_headers_lose_their_value_in_every_spelling(self) -> None:
        for name in ("X-Api-Key", "Api-Key", "X-API-KEY", "X_Api_Key", "apikey", "X-Auth-Key", "Cookie", "Set-Cookie", "X.Api.Key"):
            with self.subTest(header=name):
                self.assertEqual(redact.redact(redact.KIND_HEADER, f"{name}: hunter2"), f"{name}: [REDACTED]")

    def test_header_lines_that_only_contain_the_letters_pin_are_not_redacted(self) -> None:
        for name in ("Shipping-Notes", "Subject", "X-Spinner", "Message-ID", "X-Monkey", "Cookbook"):
            with self.subTest(header=name):
                self.assertEqual(redact.redact(redact.KIND_HEADER, f"{name}: visible"), f"{name}: visible")

    def test_auth_commands_and_imap_login_lose_everything_after_the_verb(self) -> None:
        self.assertEqual(redact.redact(redact.KIND_COMMAND, "AUTH PLAIN AHVzZXIAcGFzcw=="), "AUTH PLAIN [REDACTED]")
        self.assertEqual(redact.redact(redact.KIND_COMMAND, "auth login"), "AUTH LOGIN [REDACTED]")
        self.assertEqual(redact.redact(redact.KIND_COMMAND, 'a1 LOGIN user "pass word"'), "a1 LOGIN [REDACTED]")

    def test_an_answer_to_an_auth_challenge_is_always_the_marker(self) -> None:
        self.assertEqual(redact.redact(redact.KIND_AUTH_DATA, "cGFzc3dvcmQ="), redact.MARKER)

    def test_an_ordinary_command_is_logged_as_it_is(self) -> None:
        self.assertEqual(redact.redact(redact.KIND_COMMAND, "MAIL FROM:<a@acme-example.test>"), "MAIL FROM:<a@acme-example.test>")
