---
name: mail-mock
description: Test that your system sends the right mail without a real mail server. Runs an SMTP relay (STARTTLS, AUTH PLAIN and LOGIN) and an IMAP mailbox per test run; send with smtplib, find the caught mail by Message-ID with imaplib, assert on the message.delivered event, and inject refusals, 451 failures and delays. Use when code under test sends e-mail, or when a check needs a recipient that rejects, defers or is slow.
license: Apache-2.0
metadata:
  saphan.origin: mocks-studio
  saphan.contract: mocks.adapter/v1
  saphan.mock: mail-mock
---

# mail-mock: a mail relay and a mailbox for tests

Mail your system sends is caught, not delivered. Each **run** gets its own SMTP listener, its own IMAP
listener and its own empty mailbox, so two runs never see each other's mail. All names below are fictional
(`@acme-example.test`).

## Start it

```
python -m mail_mock serve --control 127.0.0.1:19100
```

The control endpoint (`/_mock/v1`, `spec/MOCK_ADAPTER_V1.md` section 4) is for the core and for setup. Your
code under test talks to the **SMTP port** it is given; your test reads back from the **IMAP port**.

## Open a run

`POST /_mock/v1/scopes` with `{"run_id": "<run>", "listener": {"protocol": "smtp", "host": "127.0.0.1", "port": 0}}`
(`port: 0` lets the mock choose; the core assigns one in real use). Protocol `smtp` is a plain SMTP listener.
Protocol `tcp` is the same listener that also answers the conformance probe line; use `smtp` for real mail.

The `201` answer carries, beside the spec's `run_id` and `listener`:

| field | meaning |
|---|---|
| `listener.port` | the SMTP port of this run |
| `imap.port` | the IMAP port of this run (same host) |
| `tls_certificate_pem` | the run's self-signed certificate, made when the run opened; trust exactly this |

Close with `DELETE /_mock/v1/scopes/<run>`: both ports stop answering and open sessions are cut.

## What it speaks

**SMTP** (RFC 5321): `EHLO`, `HELO`, `MAIL FROM`, `RCPT TO`, `DATA`, `RSET`, `NOOP`, `QUIT`, `STARTTLS`
(RFC 3207), `AUTH PLAIN` and `AUTH LOGIN` (RFC 4954). `VRFY` and `EXPN` answer 502. Credentials default to
`agent@acme-example.test` and a fictional password; change them with the settings `auth_user` and
`auth_password`. AUTH is accepted with or without STARTTLS. A mail without a `Message-ID` is given one, as a
relay does. Size limit 10 MiB (`552`).

**IMAP** (RFC 3501 subset, INBOX only): `LOGIN` (same credentials), `CAPABILITY`, `NOOP`, `SELECT INBOX`,
`EXAMINE INBOX`, `SEARCH ALL`, `SEARCH HEADER <name> <value>` (case-insensitive substring), `FETCH` of
`RFC822`, `BODY[]`, `BODY.PEEK[]`, `ENVELOPE`, `FLAGS`, `UID`, `RFC822.SIZE`, `LOGOUT`. Message numbers are
arrival order; mail that arrives after `SELECT` is visible at once. Anything else is answered `BAD`.

**Refusals you can rely on** (each is tested): wrong credentials `535` (SMTP) / `NO [AUTHENTICATIONFAILED]`
(IMAP); a command out of order `503` / `BAD`; bad syntax `501`; unknown command `500`.

## Settings (set with `PUT /_mock/v1/scopes/<run>/settings`, undone by `POST .../reset`)

| setting | type, default | effect |
|---|---|---|
| `refuse_recipient` | string, `""` | comma-separated addresses answered `550` at `RCPT TO` (case-insensitive); other recipients succeed |
| `auth_required` | boolean, `false` | `MAIL FROM` before a successful AUTH is answered `530` |
| `smtp_delay_ms` | integer 0..10000, `0` | every SMTP reply after the greeting is held back this long |
| `tempfail` | boolean, `false` | every `RCPT TO` is answered `451` |
| `auth_user`, `auth_password` | string | the credentials AUTH and IMAP LOGIN accept |

Each setting that changes an answer also emits `fault.injected`. Reset returns the settings to their
defaults and keeps the mailbox.

## Events (`GET /_mock/v1/events?run_id=<run>`, server-sent events)

- `message.delivered`: one per accepted mail. `correlation_keys` is `{"message_id": "<Message-ID>"}`,
  `correlation_source` is `correlation-key`, and `payload.fields` holds `from`, `to`, `subject`, `message_id`
  (the safe fields; never a body or a credential). The same Message-ID always yields the same `correlation_id`.
- `request.received` / `response.sent`: every SMTP command and reply, every IMAP command and completion.
  Their payloads are redacted (a digest and a size).
- `fault.injected`, `scenario.set` (names the settings, never their values), `scope.opened`, `scope.reset`,
  `scope.closed`.

**No secret is logged or exported**: a password, the base64 `AUTH PLAIN` string, and the value of any mail
header named like `password`, `pin`, `token`, `secret` or `authorization` never reach the log, stdout, stderr
or an event; the log keeps the header name and `[REDACTED]`.

## Worked example

Send with `smtplib` (STARTTLS and AUTH), find the mail by Message-ID with `imaplib`, assert on the event.

```python
import imaplib
import json
import smtplib
import ssl
import urllib.request
from email.message import EmailMessage
from email.utils import make_msgid

CONTROL = "http://127.0.0.1:19100"


def control(method, path, body=None):
    data = None if body is None else json.dumps(body).encode()
    request = urllib.request.Request(CONTROL + path, data=data, method=method, headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(request, timeout=5) as response:
        raw = response.read()
        return json.loads(raw) if raw else None


run = control("POST", "/_mock/v1/scopes", {"run_id": "demo-run-1", "listener": {"protocol": "smtp", "host": "127.0.0.1", "port": 0}})
smtp_port, imap_port = run["listener"]["port"], run["imap"]["port"]

# Watch the run's events before anything is sent: the stream starts at the moment it is opened.
stream = urllib.request.urlopen(CONTROL + "/_mock/v1/events?run_id=demo-run-1", timeout=5)

message = EmailMessage()
message["From"] = "shop@acme-example.test"
message["To"] = "customer@acme-example.test"
message["Subject"] = "Your order 1234"
message["Message-ID"] = make_msgid(domain="acme-example.test")
message.set_content("Thank you for your order.\n")

trust = ssl.create_default_context(cadata=run["tls_certificate_pem"])
with smtplib.SMTP("127.0.0.1", smtp_port, timeout=5) as smtp:
    smtp.ehlo()
    smtp.starttls(context=trust)
    smtp.ehlo()
    smtp.login("agent@acme-example.test", "fictional-password-for-tests")
    smtp.send_message(message)

mailbox = imaplib.IMAP4("127.0.0.1", imap_port, timeout=5)
mailbox.login("agent@acme-example.test", "fictional-password-for-tests")
mailbox.select("INBOX")
_, found = mailbox.search(None, "HEADER", "Message-ID", '"' + message["Message-ID"] + '"')
assert found == [b"1"], found
_, fetched = mailbox.fetch("1", "(RFC822)")
assert b"Subject: Your order 1234" in fetched[0][1]
mailbox.logout()

event = None
while event is None:
    line = stream.readline().decode()
    if line.startswith("data: ") and json.loads(line[6:])["kind"] == "message.delivered":
        event = json.loads(line[6:])
assert event["correlation_keys"] == {"message_id": message["Message-ID"]}
assert event["payload"]["fields"]["subject"] == "Your order 1234"
stream.close()

control("DELETE", "/_mock/v1/scopes/demo-run-1")
print("caught", message["Message-ID"])
```

To prove a failure path, set `{"refuse_recipient": "blocked@acme-example.test"}` before sending: that recipient
fails with `550` (`smtplib.SMTPRecipientsRefused` when it is the only one) and the others still succeed.

## Check the mock itself

```
python -m unittest discover -s mocks/mail -v
go -C conformance run ./cmd/mocks-conformance --manifest mocks/mail/mocks.manifest.json
```

## Limits (read before trusting a green run)

- The conformance suite drives only `http` and `tcp` listeners. This mock declares `smtp` and `tcp`: the suite
  judges the control surface, scopes, settings and events through the `tcp` probe (a line sent first is
  answered with one status line that reflects the settings); the SMTP and IMAP behaviour is proved by this
  mock's own tests, not by the suite.
- Of the settings, the suite exercises the first one that declares a `tcp` effect (`refuse_recipient`); the
  others are proved by this mock's own tests.
- Not here: POP3, IMAP folders other than INBOX, IDLE, `UID SEARCH`/`UID FETCH`, literals in IMAP commands,
  DKIM/SPF, delivery to any real host.
