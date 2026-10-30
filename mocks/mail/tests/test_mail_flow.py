"""A mail sent with smtplib is read back with imaplib by its Message-ID, and the events say so."""

from __future__ import annotations

import imaplib
import smtplib
import time

from mail_mock import manifest

from .support import HOST, WAIT_SECONDS, MockTestCase, plain_credentials


def imap_find(scope, message_id: str) -> tuple[list[bytes], imaplib.IMAP4]:
    """Log in to the scope's IMAP side and search INBOX by Message-ID; returns the hits and the open client."""
    user, password = plain_credentials()
    client = imaplib.IMAP4(HOST, scope.imap_port, timeout=WAIT_SECONDS)
    client.login(user, password)
    client.select("INBOX")
    typ, data = client.search(None, "HEADER", "Message-ID", f'"{message_id}"')
    assert typ == "OK", typ
    return data[0].split(), client


class MailFlowTest(MockTestCase):
    def test_mail_sent_with_starttls_and_auth_is_read_back_by_message_id(self) -> None:
        scope = self.open_scope("flow-run-one")
        msg = self.message(subject="Your order 1234", body="Line one\n.leading dot line\nLine three\n")
        user, password = plain_credentials()
        with smtplib.SMTP(HOST, scope.port, timeout=WAIT_SECONDS) as smtp:
            smtp.ehlo()
            smtp.starttls(context=self.trust(scope))
            smtp.ehlo()
            smtp.login(user, password)
            self.assertEqual(smtp.send_message(msg), {})
        hits, client = imap_find(scope, msg["Message-ID"])
        self.assertEqual(hits, [b"1"])
        typ, data = client.fetch("1", "(RFC822)")
        client.logout()
        self.assertEqual(typ, "OK")
        stored = data[0][1].decode("utf-8")
        self.assertIn("Subject: Your order 1234", stored)
        self.assertIn("\r\n.leading dot line\r\n", stored, "dot-stuffing must be undone on receipt")
        delivered = self.tap.of_kind("message.delivered")
        self.assertEqual(len(delivered), 1)
        event = delivered[0]
        self.assertEqual(event["correlation_keys"], {"message_id": msg["Message-ID"]})
        self.assertEqual(event["correlation_source"], "correlation-key")
        self.assertEqual(event["run_id"], "flow-run-one")
        self.assertEqual(
            event["payload"],
            {"redacted": False, "fields": {
                "from": "shop@acme-example.test", "to": "customer@acme-example.test",
                "subject": "Your order 1234", "message_id": msg["Message-ID"],
            }},
        )

    def test_one_message_id_always_yields_the_same_correlation_id(self) -> None:
        scope = self.open_scope("flow-run-two")
        msg = self.message()
        with smtplib.SMTP(HOST, scope.port, timeout=WAIT_SECONDS) as smtp:
            smtp.send_message(msg)
            smtp.send_message(msg)
        ids = {e["correlation_id"] for e in self.tap.of_kind("message.delivered")}
        self.assertEqual(len(ids), 1)

    def test_mail_without_message_id_is_given_one_and_can_be_found_by_it(self) -> None:
        scope = self.open_scope("flow-run-three")
        with smtplib.SMTP(HOST, scope.port, timeout=WAIT_SECONDS) as smtp:
            smtp.sendmail("a@acme-example.test", ["b@acme-example.test"], "Subject: bare\r\n\r\nno id\r\n")
        generated = self.tap.of_kind("message.delivered")[0]["correlation_keys"]["message_id"]
        hits, client = imap_find(scope, generated)
        client.logout()
        self.assertEqual(hits, [b"1"])

    def test_a_mail_in_scope_a_is_not_in_scope_b(self) -> None:
        a, b = self.open_scope("flow-scope-a"), self.open_scope("flow-scope-b")
        msg = self.message()
        with smtplib.SMTP(HOST, a.port, timeout=WAIT_SECONDS) as smtp:
            smtp.send_message(msg)
        hits_a, client_a = imap_find(a, msg["Message-ID"])
        hits_b, client_b = imap_find(b, msg["Message-ID"])
        typ, all_b = client_b.search(None, "ALL")
        client_a.logout()
        client_b.logout()
        self.assertEqual(hits_a, [b"1"])
        self.assertEqual(hits_b, [])
        self.assertEqual(all_b, [b""], "scope B's mailbox must be empty")
        self.assertEqual({e["run_id"] for e in self.tap.of_kind("message.delivered")}, {"flow-scope-a"})

    def test_auth_login_mechanism_also_authenticates(self) -> None:
        scope = self.open_scope("flow-run-login")
        user, password = plain_credentials()
        with smtplib.SMTP(HOST, scope.port, timeout=WAIT_SECONDS) as smtp:
            smtp.ehlo()
            smtp.user, smtp.password = user, password
            code, _ = smtp.auth("LOGIN", smtp.auth_login)
            self.assertEqual(code, 235)

    def test_credentials_set_by_setting_replace_the_defaults(self) -> None:
        scope = self.open_scope("flow-run-creds")
        self.set_settings(scope, {manifest.AUTH_USER: "other@acme-example.test", manifest.AUTH_PASSWORD: "another-fictional-pw"})
        with smtplib.SMTP(HOST, scope.port, timeout=WAIT_SECONDS) as smtp:
            smtp.ehlo()
            smtp.login("other@acme-example.test", "another-fictional-pw")
        with smtplib.SMTP(HOST, scope.port, timeout=WAIT_SECONDS) as smtp:
            smtp.ehlo()
            with self.assertRaises(smtplib.SMTPAuthenticationError):
                smtp.login(*plain_credentials())

    def test_refuse_recipient_makes_that_recipient_fail_with_550_and_the_others_succeed(self) -> None:
        scope = self.open_scope("flow-run-refuse")
        self.set_settings(scope, {manifest.REFUSE_RECIPIENT: "Blocked@acme-example.test"})
        msg = self.message()
        with smtplib.SMTP(HOST, scope.port, timeout=WAIT_SECONDS) as smtp:
            refused = smtp.sendmail(
                "shop@acme-example.test",
                ["blocked@acme-example.test", "fine@acme-example.test"],
                msg.as_bytes(),
            )
        self.assertEqual(list(refused), ["blocked@acme-example.test"])
        self.assertEqual(refused["blocked@acme-example.test"][0], 550)
        delivered = self.tap.of_kind("message.delivered")
        self.assertEqual(len(delivered), 1)
        self.assertEqual(delivered[0]["payload"]["fields"]["to"], "fine@acme-example.test")
        faults = self.tap.of_kind("fault.injected")
        self.assertEqual([f["fault"] for f in faults], [manifest.REFUSE_RECIPIENT])

    def test_refuse_recipient_refused_when_it_is_the_only_recipient_delivers_nothing(self) -> None:
        scope = self.open_scope("flow-run-refuse-only")
        self.set_settings(scope, {manifest.REFUSE_RECIPIENT: "blocked@acme-example.test"})
        with smtplib.SMTP(HOST, scope.port, timeout=WAIT_SECONDS) as smtp:
            with self.assertRaises(smtplib.SMTPRecipientsRefused):
                smtp.sendmail("shop@acme-example.test", ["blocked@acme-example.test"], self.message().as_bytes())
        self.assertEqual(self.tap.of_kind("message.delivered"), [])
        self.assertEqual(len(scope.mailbox), 0)

    def test_tempfail_answers_every_rcpt_451_and_reset_restores_delivery(self) -> None:
        scope = self.open_scope("flow-run-tempfail")
        self.set_settings(scope, {manifest.TEMPFAIL: True})
        with smtplib.SMTP(HOST, scope.port, timeout=WAIT_SECONDS) as smtp:
            with self.assertRaises(smtplib.SMTPRecipientsRefused) as caught:
                smtp.sendmail("a@acme-example.test", ["b@acme-example.test"], self.message().as_bytes())
            self.assertEqual(caught.exception.recipients["b@acme-example.test"][0], 451)
            self.mock.reset(scope.run_id)
            smtp.sendmail("a@acme-example.test", ["b@acme-example.test"], self.message().as_bytes())
        self.assertEqual(len(scope.mailbox), 1)

    def test_auth_required_refuses_mail_with_530_until_auth_succeeds(self) -> None:
        scope = self.open_scope("flow-run-authreq")
        self.set_settings(scope, {manifest.AUTH_REQUIRED: True})
        with smtplib.SMTP(HOST, scope.port, timeout=WAIT_SECONDS) as smtp:
            smtp.ehlo()
            self.assertEqual(smtp.mail("shop@acme-example.test")[0], 530)
            smtp.login(*plain_credentials())
            self.assertEqual(smtp.mail("shop@acme-example.test")[0], 250)

    def test_smtp_delay_ms_holds_each_reply_and_emits_fault_injected(self) -> None:
        scope = self.open_scope("flow-run-delay")
        self.set_settings(scope, {manifest.SMTP_DELAY_MS: 300})
        with smtplib.SMTP(HOST, scope.port, timeout=WAIT_SECONDS) as smtp:
            started = time.monotonic()
            smtp.noop()
            elapsed = time.monotonic() - started
        self.assertGreaterEqual(elapsed, 0.28)
        self.assertIn(manifest.SMTP_DELAY_MS, [e["fault"] for e in self.tap.of_kind("fault.injected")])

    def test_events_of_a_session_validate_and_carry_the_run_and_no_unsafe_field(self) -> None:
        scope = self.open_scope("flow-run-events")
        with smtplib.SMTP(HOST, scope.port, timeout=WAIT_SECONDS) as smtp:
            smtp.send_message(self.message())
        events = self.tap.drain()  # drain validates each against the published schema
        self.assertTrue({"scope.opened", "request.received", "response.sent", "message.delivered"} <= {e["kind"] for e in events})
        for event in events:
            self.assertEqual(event["run_id"], "flow-run-events")
            fields = (event.get("payload") or {}).get("fields", {})
            self.assertLessEqual(set(fields), set(manifest.SAFE_FIELDS))
