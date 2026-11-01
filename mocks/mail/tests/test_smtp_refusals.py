"""Every SMTP refusal the mock implements, each test breaking exactly one thing (named in the test)."""

from __future__ import annotations

import base64

from mail_mock import manifest
from mail_mock.smtp import MAX_MESSAGE_BYTES

from .support import MockTestCase, RawSmtp, plain_credentials


def b64(text: str) -> str:
    return base64.b64encode(text.encode("utf-8")).decode("ascii")


class SmtpRefusalTest(MockTestCase):
    def setUp(self) -> None:
        super().setUp()
        self.scope = self.open_scope("smtp-refusals")

    def client(self) -> RawSmtp:
        raw = RawSmtp(self.scope.port)
        self.addCleanup(raw.close)
        return raw

    def greeted(self) -> RawSmtp:
        raw = self.client()
        self.assertEqual(raw.ehlo()[0], 250)
        return raw

    def in_transaction(self) -> RawSmtp:
        raw = self.greeted()
        self.assertEqual(raw.cmd("MAIL FROM:<a@acme-example.test>")[0], 250)
        return raw

    def plain(self, user: str, password: str) -> str:
        return "AUTH PLAIN " + b64(f"\0{user}\0{password}")

    # -- AUTH --------------------------------------------------------------------------------

    def test_auth_plain_with_one_wrong_password_is_refused_535(self) -> None:
        user, _ = plain_credentials()
        self.assertEqual(self.greeted().cmd(self.plain(user, "wrong-password"))[0], 535)

    def test_auth_plain_with_one_wrong_user_is_refused_535(self) -> None:
        _, password = plain_credentials()
        self.assertEqual(self.greeted().cmd(self.plain("nobody@acme-example.test", password))[0], 535)

    def test_auth_plain_with_correct_credentials_is_accepted_235(self) -> None:
        self.assertEqual(self.greeted().cmd(self.plain(*plain_credentials()))[0], 235)

    def test_auth_login_with_one_wrong_password_is_refused_535(self) -> None:
        user, _ = plain_credentials()
        raw = self.greeted()
        self.assertEqual(raw.cmd("AUTH LOGIN")[0], 334)
        self.assertEqual(raw.cmd(b64(user))[0], 334)
        self.assertEqual(raw.cmd(b64("wrong-password"))[0], 535)

    def test_auth_login_cancelled_with_star_is_refused_501(self) -> None:
        raw = self.greeted()
        self.assertEqual(raw.cmd("AUTH LOGIN")[0], 334)
        self.assertEqual(raw.cmd("*")[0], 501)

    def test_auth_plain_with_one_undecodable_base64_is_refused_501(self) -> None:
        self.assertEqual(self.greeted().cmd("AUTH PLAIN !!!not-base64!!!")[0], 501)

    def test_auth_plain_with_one_field_missing_is_refused_501(self) -> None:
        self.assertEqual(self.greeted().cmd("AUTH PLAIN " + b64("only-one-field"))[0], 501)

    def test_auth_with_one_unknown_mechanism_is_refused_504(self) -> None:
        self.assertEqual(self.greeted().cmd("AUTH CRAM-MD5")[0], 504)

    def test_auth_before_ehlo_is_refused_503(self) -> None:
        self.assertEqual(self.client().cmd(self.plain(*plain_credentials()))[0], 503)

    def test_a_second_auth_after_success_is_refused_503(self) -> None:
        raw = self.greeted()
        self.assertEqual(raw.cmd(self.plain(*plain_credentials()))[0], 235)
        self.assertEqual(raw.cmd(self.plain(*plain_credentials()))[0], 503)

    def test_auth_inside_a_mail_transaction_is_refused_503(self) -> None:
        self.assertEqual(self.in_transaction().cmd(self.plain(*plain_credentials()))[0], 503)

    # -- sequence ----------------------------------------------------------------------------

    def test_mail_from_before_ehlo_is_refused_503(self) -> None:
        self.assertEqual(self.client().cmd("MAIL FROM:<a@acme-example.test>")[0], 503)

    def test_rcpt_to_without_mail_from_is_refused_503(self) -> None:
        self.assertEqual(self.greeted().cmd("RCPT TO:<b@acme-example.test>")[0], 503)

    def test_data_without_mail_from_is_refused_503(self) -> None:
        self.assertEqual(self.greeted().cmd("DATA")[0], 503)

    def test_data_without_an_accepted_rcpt_is_refused_503(self) -> None:
        self.assertEqual(self.in_transaction().cmd("DATA")[0], 503)

    def test_a_nested_mail_from_is_refused_503(self) -> None:
        self.assertEqual(self.in_transaction().cmd("MAIL FROM:<c@acme-example.test>")[0], 503)

    def test_rset_clears_the_transaction_so_rcpt_to_is_then_refused_503(self) -> None:
        raw = self.in_transaction()
        self.assertEqual(raw.cmd("RSET")[0], 250)
        self.assertEqual(raw.cmd("RCPT TO:<b@acme-example.test>")[0], 503)

    def test_starttls_twice_is_refused_503(self) -> None:
        raw = self.greeted()
        raw.starttls(self.trust(self.scope))
        self.assertEqual(raw.ehlo()[0], 250)
        self.assertEqual(raw.cmd("STARTTLS")[0], 503)

    def test_starttls_with_an_argument_is_refused_501(self) -> None:
        self.assertEqual(self.greeted().cmd("STARTTLS now")[0], 501)

    def test_starttls_resets_the_session_so_mail_from_without_a_new_ehlo_is_refused_503(self) -> None:
        raw = self.greeted()
        raw.starttls(self.trust(self.scope))
        self.assertEqual(raw.cmd("MAIL FROM:<a@acme-example.test>")[0], 503)

    # -- syntax ------------------------------------------------------------------------------

    def test_ehlo_without_a_name_is_refused_501(self) -> None:
        self.assertEqual(self.client().cmd("EHLO")[0], 501)

    def test_mail_from_without_angle_brackets_is_refused_501(self) -> None:
        self.assertEqual(self.greeted().cmd("MAIL FROM:a@acme-example.test")[0], 501)

    def test_mail_from_with_an_address_that_has_no_at_sign_is_refused_501(self) -> None:
        self.assertEqual(self.greeted().cmd("MAIL FROM:<not-an-address>")[0], 501)

    def test_mail_from_with_an_unknown_parameter_is_refused_555(self) -> None:
        self.assertEqual(self.greeted().cmd("MAIL FROM:<a@acme-example.test> FOO=BAR")[0], 555)

    def test_mail_from_declaring_a_size_over_the_limit_is_refused_552(self) -> None:
        self.assertEqual(self.greeted().cmd(f"MAIL FROM:<a@acme-example.test> SIZE={MAX_MESSAGE_BYTES + 1}")[0], 552)

    def test_the_null_sender_is_accepted(self) -> None:
        self.assertEqual(self.greeted().cmd("MAIL FROM:<>")[0], 250)

    def test_rcpt_to_without_angle_brackets_is_refused_501(self) -> None:
        self.assertEqual(self.in_transaction().cmd("RCPT TO:b@acme-example.test")[0], 501)

    def test_rcpt_to_with_an_address_that_has_no_at_sign_is_refused_501(self) -> None:
        self.assertEqual(self.in_transaction().cmd("RCPT TO:<nobody>")[0], 501)

    def test_data_with_an_argument_is_refused_501(self) -> None:
        self.assertEqual(self.in_transaction().cmd("DATA now")[0], 501)

    def test_an_unknown_command_is_refused_500(self) -> None:
        self.assertEqual(self.greeted().cmd("FROBNICATE")[0], 500)

    def test_vrfy_is_refused_502(self) -> None:
        self.assertEqual(self.greeted().cmd("VRFY someone")[0], 502)

    def test_quit_is_answered_221_and_the_connection_closes(self) -> None:
        raw = self.greeted()
        self.assertEqual(raw.cmd("QUIT")[0], 221)
        with self.assertRaises(ConnectionError):
            raw.read()

    # -- injected faults, one setting each -----------------------------------------------------

    def test_auth_required_alone_refuses_mail_from_530_and_leaves_ehlo_and_noop_alone(self) -> None:
        self.set_settings(self.scope, {manifest.AUTH_REQUIRED: True})
        raw = self.greeted()
        self.assertEqual(raw.cmd("NOOP")[0], 250)
        self.assertEqual(raw.cmd("MAIL FROM:<a@acme-example.test>")[0], 530)

    def test_tempfail_alone_refuses_rcpt_451_and_leaves_mail_from_alone(self) -> None:
        self.set_settings(self.scope, {manifest.TEMPFAIL: True})
        raw = self.in_transaction()
        self.assertEqual(raw.cmd("RCPT TO:<b@acme-example.test>")[0], 451)

    def test_refuse_recipient_alone_refuses_only_the_named_address_550(self) -> None:
        self.set_settings(self.scope, {manifest.REFUSE_RECIPIENT: "no@acme-example.test, also-no@acme-example.test"})
        raw = self.in_transaction()
        self.assertEqual(raw.cmd("RCPT TO:<ALSO-NO@acme-example.test>")[0], 550)
        self.assertEqual(raw.cmd("RCPT TO:<yes@acme-example.test>")[0], 250)

    def test_a_message_over_the_size_limit_is_refused_552_and_not_stored(self) -> None:
        raw = self.in_transaction()
        self.assertEqual(raw.cmd("RCPT TO:<b@acme-example.test>")[0], 250)
        self.assertEqual(raw.cmd("DATA")[0], 354)
        line = b"x" * 900 + b"\r\n"
        chunk = line * 1000
        sent = 0
        raw.sock.settimeout(30)
        while sent <= MAX_MESSAGE_BYTES:
            raw.sock.sendall(chunk)
            sent += len(chunk)
        raw.sock.sendall(b".\r\n")
        self.assertEqual(raw.read()[0], 552)
        self.assertEqual(len(self.scope.mailbox), 0)

    # -- DATA lines: read whole up to 1000 octets with the CRLF (RFC 5321 4.5.3.1.6); dot rule 4.5.2 --

    def into_data(self) -> RawSmtp:
        raw = self.in_transaction()
        self.assertEqual(raw.cmd("RCPT TO:<b@acme-example.test>")[0], 250)
        self.assertEqual(raw.cmd("DATA")[0], 354)
        return raw

    def stored_body(self) -> bytes:
        (mail,) = self.scope.mailbox.snapshot()
        return mail.raw.split(b"\r\n\r\n", 1)[1]

    def test_a_line_of_exactly_the_read_size_followed_by_a_lone_dot_does_not_end_data_early(self) -> None:
        raw = self.into_data()
        raw.sock.sendall(b"A" * 1001 + b".\r\nQUIT\r\nmore body\r\n.\r\n")
        self.assertEqual(raw.read()[0], 500)
        # the piece after 1001 octets is `.\r\n`: it is body and must not end DATA, or QUIT would run and answer 221 here
        self.assertEqual(raw.cmd("NOOP")[0], 250)
        self.assertEqual(len(self.scope.mailbox), 0)

    def test_a_line_whose_second_piece_is_a_lone_dot_does_not_end_data_early(self) -> None:
        raw = self.into_data()
        raw.sock.sendall(b"A" * 1002 + b".\r\nRSET\r\nmore body\r\n.\r\n")
        self.assertEqual(raw.read()[0], 500)
        self.assertEqual(raw.cmd("NOOP")[0], 250)  # one reply for the whole DATA: RSET and "more body" were body, not commands
        self.assertEqual(len(self.scope.mailbox), 0)

    def test_a_line_over_the_limit_is_refused_500_and_never_stored_split(self) -> None:
        raw = self.into_data()
        raw.sock.sendall(b"x" * 1500 + b"\r\n.\r\n")
        self.assertEqual(raw.read()[0], 500)
        self.assertEqual(len(self.scope.mailbox), 0)

    def test_a_line_of_exactly_1000_octets_with_its_crlf_is_stored_intact(self) -> None:
        raw = self.into_data()
        line = b"y" * 998
        raw.sock.sendall(b"Subject: s\r\n\r\n" + line + b"\r\n.\r\n")
        self.assertEqual(raw.read()[0], 250)
        self.assertEqual(self.stored_body(), line + b"\r\n")

    def test_a_line_of_1001_octets_with_its_crlf_is_refused_500(self) -> None:
        raw = self.into_data()
        raw.sock.sendall(b"Subject: s\r\n\r\n" + b"y" * 999 + b"\r\n.\r\n")
        self.assertEqual(raw.read()[0], 500)
        self.assertEqual(len(self.scope.mailbox), 0)

    def test_a_line_starting_with_one_dot_loses_that_dot(self) -> None:
        raw = self.into_data()
        raw.sock.sendall(b"Subject: s\r\n\r\n.hidden\r\n..doubled\r\n.\r\n")
        self.assertEqual(raw.read()[0], 250)
        self.assertEqual(self.stored_body(), b"hidden\r\n.doubled\r\n")

    def test_a_line_ending_in_two_crs_is_stored_with_both(self) -> None:
        raw = self.into_data()
        raw.sock.sendall(b"Subject: s\r\n\r\ntext\r\r\n.\r\n")
        self.assertEqual(raw.read()[0], 250)
        self.assertEqual(self.stored_body(), b"text\r\r\n")
