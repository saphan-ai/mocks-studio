# mail-mock

A mock of a mail relay and mailbox for `mocks.adapter/v1`: SMTP (STARTTLS, AUTH PLAIN/LOGIN) catches the
mail, IMAP reads it back by Message-ID. Python 3.12, standard library plus `cryptography` (certificates).

    cd mocks/mail   # from the repository root
    pip install -r requirements.txt
    python -m mail_mock serve --control 127.0.0.1:19100

- Test: `python -m unittest discover -s mocks/mail -v` (from the repository root).
- Judge: `go -C conformance run ./cmd/mocks-conformance --manifest ../mocks/mail/mocks.manifest.json` (from the repository root).
- An agent reads `SKILL.md`; the contract is `spec/MOCK_ADAPTER_V1.md`.
- Each scope has its own mailbox, SMTP and IMAP listeners; its self-signed certificate is never written anywhere that lasts.
- Passwords, AUTH strings and secret-named headers are never logged and never in an event.
- Fictional names only (`@acme-example.test`); POP3, folders other than INBOX, IDLE, DKIM/SPF: not here.
