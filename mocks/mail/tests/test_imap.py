"""The IMAP subset: reading mail back, and each refusal with exactly one thing broken."""

from __future__ import annotations

import imaplib
import smtplib

from .support import HOST, WAIT_SECONDS, MockTestCase, RawSmtp, plain_credentials


class ImapTest(MockTestCase):
    def setUp(self) -> None:
        super().setUp()
        self.scope = self.open_scope("imap-run")

    def deliver(self, **kw) -> str:
        msg = self.message(**kw)
        with smtplib.SMTP(HOST, self.scope.port, timeout=WAIT_SECONDS) as smtp:
            smtp.send_message(msg)
        return msg["Message-ID"]

    def client(self, *, login: bool = True, select: bool = True) -> imaplib.IMAP4:
        client = imaplib.IMAP4(HOST, self.scope.imap_port, timeout=WAIT_SECONDS)
        self.addCleanup(self._logout, client)
        if login:
            client.login(*plain_credentials())
        if select:
            client.select("INBOX")
        return client

    @staticmethod
    def _logout(client: imaplib.IMAP4) -> None:
        try:
            client.logout()
        except (imaplib.IMAP4.error, OSError):
            pass  # a refused session may already be closed

    def raw(self) -> "RawImap":
        raw = RawImap(self.scope.imap_port)
        self.addCleanup(raw.close)
        return raw

    # -- reading back -----------------------------------------------------------------------

    def test_search_all_lists_every_mail_in_arrival_order(self) -> None:
        self.deliver(subject="first")
        self.deliver(subject="second")
        typ, data = self.client().search(None, "ALL")
        self.assertEqual((typ, data), ("OK", [b"1 2"]))

    def test_search_header_message_id_finds_exactly_that_mail(self) -> None:
        self.deliver(subject="first")
        wanted = self.deliver(subject="second")
        typ, data = self.client().search(None, "HEADER", "Message-ID", f'"{wanted}"')
        self.assertEqual(data, [b"2"])

    def test_search_header_message_id_with_an_unknown_id_finds_nothing(self) -> None:
        self.deliver()
        typ, data = self.client().search(None, "HEADER", "Message-ID", '"<nobody@acme-example.test>"')
        self.assertEqual((typ, data), ("OK", [b""]))

    def test_fetch_rfc822_returns_the_whole_message(self) -> None:
        mid = self.deliver(subject="whole", body="body text\n")
        typ, data = self.client().fetch("1", "(RFC822)")
        self.assertEqual(typ, "OK")
        self.assertIn(f"Message-ID: {mid}".encode(), data[0][1])
        self.assertTrue(data[0][1].endswith(b"body text\r\n"))

    def test_fetch_body_peek_empty_section_returns_the_whole_message(self) -> None:
        self.deliver(subject="peek")
        typ, data = self.client().fetch("1", "(BODY.PEEK[])")
        self.assertIn(b"Subject: peek", data[0][1])

    def test_fetch_envelope_carries_subject_addresses_and_message_id(self) -> None:
        mid = self.deliver(subject="env")
        typ, data = self.client().fetch("1", "(ENVELOPE)")
        text = data[0].decode() if isinstance(data[0], bytes) else data[0][0].decode()
        for expected in ('"env"', '"shop"', '"acme-example.test"', '"customer"', mid):
            self.assertIn(expected, text)

    def deliver_raw(self, subject: bytes) -> None:
        """Send one mail whose Subject header is exactly `subject` (bytes), over a byte-exact SMTP session."""
        with RawSmtp(self.scope.port) as smtp:
            smtp.ehlo()
            self.assertEqual(smtp.cmd("MAIL FROM:<a@acme-example.test>")[0], 250)
            self.assertEqual(smtp.cmd("RCPT TO:<b@acme-example.test>")[0], 250)
            self.assertEqual(smtp.cmd("DATA")[0], 354)
            smtp.sock.sendall(b"From: a@acme-example.test\r\nTo: b@acme-example.test\r\nSubject: " + subject + b"\r\n\r\nbody\r\n.\r\n")
            self.assertEqual(smtp.read()[0], 250)

    def envelope_bytes(self, client: imaplib.IMAP4) -> bytes:
        typ, data = client.fetch("1", "(ENVELOPE)")
        self.assertEqual(typ, "OK")
        return b"".join(part for item in data for part in (item if isinstance(item, tuple) else (item,)) if isinstance(part, bytes))

    def test_envelope_sends_an_rfc2047_subject_as_the_raw_encoded_words(self) -> None:
        self.deliver_raw(b"=?utf-8?q?caf=C3=A9?=")
        self.assertIn(b"=?utf-8?q?caf=C3=A9?=", self.envelope_bytes(self.client()))

    def test_a_raw_8_bit_subject_is_sent_as_its_own_bytes_and_the_session_survives(self) -> None:
        self.deliver_raw(b"caf\xe9 \xc3\xa9")
        client = self.client()
        self.assertIn(b"caf\xe9 \xc3\xa9", self.envelope_bytes(client))
        self.assertEqual(client.noop()[0], "OK")

    def test_fetch_a_range_returns_each_message(self) -> None:
        self.deliver(subject="one")
        self.deliver(subject="two")
        typ, data = self.client().fetch("1:*", "(RFC822.SIZE)")
        self.assertEqual(len([d for d in data if d]), 2)

    def test_mail_arriving_after_select_is_visible_without_selecting_again(self) -> None:
        client = self.client()
        self.assertEqual(client.search(None, "ALL")[1], [b""])
        self.deliver()
        self.assertEqual(client.search(None, "ALL")[1], [b"1"])

    def test_select_reports_the_number_of_mails_as_exists(self) -> None:
        self.deliver()
        typ, data = self.client(select=False).select("INBOX")
        self.assertEqual(data, [b"1"])

    def test_noop_after_select_reports_exists(self) -> None:
        client = self.client()
        self.deliver()
        raw = self.raw()
        raw.login()
        raw.cmd("a1 SELECT INBOX")
        lines = raw.cmd("a2 NOOP")
        self.assertIn("* 1 EXISTS", lines)
        client.noop()

    # -- refusals ---------------------------------------------------------------------------

    def test_login_with_one_wrong_password_is_refused_no(self) -> None:
        user, _ = plain_credentials()
        with self.assertRaises(imaplib.IMAP4.error) as caught:
            self.client(login=False, select=False).login(user, "wrong-password")
        self.assertIn("AUTHENTICATIONFAILED", str(caught.exception))

    def test_login_with_one_wrong_user_is_refused_no(self) -> None:
        _, password = plain_credentials()
        with self.assertRaises(imaplib.IMAP4.error):
            self.client(login=False, select=False).login("nobody@acme-example.test", password)

    def test_login_twice_is_refused_bad(self) -> None:
        client = self.client(select=False)
        with self.assertRaises(imaplib.IMAP4.error):
            client.login(*plain_credentials())

    def test_select_before_login_is_refused_bad(self) -> None:
        self.assertTrue(self.raw().cmd("a1 SELECT INBOX")[-1].startswith("a1 BAD"))

    def test_search_before_select_is_refused_bad(self) -> None:
        raw = self.raw()
        raw.login()
        self.assertTrue(raw.cmd("a2 SEARCH ALL")[-1].startswith("a2 BAD"))

    def test_fetch_before_select_is_refused_bad(self) -> None:
        raw = self.raw()
        raw.login()
        self.assertTrue(raw.cmd("a2 FETCH 1 RFC822")[-1].startswith("a2 BAD"))

    def test_select_of_a_folder_other_than_inbox_is_refused_no(self) -> None:
        raw = self.raw()
        raw.login()
        self.assertTrue(raw.cmd("a2 SELECT Archive")[-1].startswith("a2 NO"))

    def test_fetch_of_a_message_number_that_does_not_exist_is_refused_no(self) -> None:
        self.deliver()
        raw = self.raw()
        raw.login()
        raw.cmd("a2 SELECT INBOX")
        self.assertTrue(raw.cmd("a3 FETCH 7 RFC822")[-1].startswith("a3 NO"))

    def test_fetch_with_an_unsupported_item_is_refused_bad(self) -> None:
        self.deliver()
        raw = self.raw()
        raw.login()
        raw.cmd("a2 SELECT INBOX")
        self.assertTrue(raw.cmd("a3 FETCH 1 BODYSTRUCTURE")[-1].startswith("a3 BAD"))

    def test_fetch_with_a_malformed_message_set_is_refused_bad(self) -> None:
        self.deliver()
        raw = self.raw()
        raw.login()
        raw.cmd("a2 SELECT INBOX")
        self.assertTrue(raw.cmd("a3 FETCH x:y RFC822")[-1].startswith("a3 BAD"))

    def test_search_with_an_unsupported_criterion_is_refused_bad(self) -> None:
        raw = self.raw()
        raw.login()
        raw.cmd("a2 SELECT INBOX")
        self.assertTrue(raw.cmd("a3 SEARCH UNSEEN")[-1].startswith("a3 BAD"))

    def test_search_header_without_a_value_is_refused_bad(self) -> None:
        raw = self.raw()
        raw.login()
        raw.cmd("a2 SELECT INBOX")
        self.assertTrue(raw.cmd("a3 SEARCH HEADER Message-ID")[-1].startswith("a3 BAD"))

    def test_an_unknown_command_is_refused_bad(self) -> None:
        self.assertTrue(self.raw().cmd("a1 FROBNICATE")[-1].startswith("a1 BAD"))

    def test_a_line_with_a_tag_only_is_refused_bad(self) -> None:
        self.assertTrue(self.raw().cmd("a1")[-1].startswith("* BAD"))

    def test_logout_says_bye_and_closes(self) -> None:
        lines = self.raw().cmd("a1 LOGOUT")
        self.assertEqual(lines[0][:5], "* BYE")
        self.assertTrue(lines[-1].startswith("a1 OK"))


class RawImap:
    """A byte-exact IMAP client for the refusal tests."""

    def __init__(self, port: int) -> None:
        import socket

        self.sock = socket.create_connection((HOST, port), timeout=WAIT_SECONDS)
        self.file = self.sock.makefile("rb")
        self.greeting = self.file.readline().decode().rstrip("\r\n")

    def cmd(self, line: str) -> list[str]:
        """Send one command; return every line up to and including the tagged completion (or a BAD/BYE close)."""
        self.sock.sendall(line.encode() + b"\r\n")
        tag = line.split(" ", 1)[0]
        out: list[str] = []
        while True:
            raw = self.file.readline()
            if not raw:
                return out
            text = raw.decode().rstrip("\r\n")
            out.append(text)
            if text.startswith(tag + " ") or text.startswith("* BAD"):
                return out

    def login(self) -> None:
        user, password = plain_credentials()
        assert self.cmd(f'a0 LOGIN {user} "{password}"')[-1].startswith("a0 OK")

    def close(self) -> None:
        for closer in (self.file.close, self.sock.close):
            try:
                closer()
            except OSError:
                pass  # already closed
