"""`python -m mail_mock serve --control <host:port>` as a real process: control, mail, stdout, stderr, stream."""

from __future__ import annotations

import http.client
import imaplib
import json
import signal
import smtplib
import subprocess
import sys
import threading
import unittest
from pathlib import Path

from .support import HOST, WAIT_SECONDS, MockTestCase
from .test_no_secret import PASSWORD, PIN, TOKEN, USER, WRONG_PASSWORD, leaks

PACKAGE_ROOT = Path(__file__).resolve().parents[1]
#: How long a stopping process is given to exit, in seconds.
EXIT_SECONDS = 10


class CliProcessTest(MockTestCase):
    def start(self) -> tuple[subprocess.Popen, str]:
        proc = subprocess.Popen(
            [sys.executable, "-m", "mail_mock", "serve", "--control", f"{HOST}:0"],
            cwd=PACKAGE_ROOT, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
        )
        self.addCleanup(lambda: proc.poll() is None and proc.kill())
        first = proc.stdout.readline().strip()
        self.assertRegex(first, r"^mail-mock control listening on http://127\.0\.0\.1:\d+$")
        return proc, first.rsplit(":", 1)[1]

    @staticmethod
    def call(port: str, method: str, path: str, body=None):
        conn = http.client.HTTPConnection(HOST, int(port), timeout=WAIT_SECONDS)
        conn.request(method, path, None if body is None else json.dumps(body), {"Content-Type": "application/json"})
        resp = conn.getresponse()
        raw = resp.read()
        conn.close()
        return resp.status, (json.loads(raw) if raw else None)

    def stream(self, port: str, sink: list[str], opened: threading.Event) -> None:
        conn = http.client.HTTPConnection(HOST, int(port), timeout=WAIT_SECONDS)
        conn.request("GET", "/_mock/v1/events")
        resp = conn.getresponse()
        opened.set()
        try:
            while True:
                line = resp.readline().decode()
                if not line:
                    return
                sink.append(line)
        except OSError:
            pass  # the process stopped or the wait elapsed

    def test_the_process_serves_mail_and_neither_its_output_nor_its_stream_holds_a_secret(self) -> None:
        proc, port = self.start()
        self.assertEqual(self.call(port, "GET", "/_mock/v1/health"), (200, {"status": "ok"}))
        streamed: list[str] = []
        opened = threading.Event()
        threading.Thread(target=self.stream, args=(port, streamed, opened), daemon=True).start()
        self.assertTrue(opened.wait(WAIT_SECONDS))
        status, scope = self.call(port, "POST", "/_mock/v1/scopes", {"run_id": "cli-run", "listener": {"protocol": "smtp", "host": HOST, "port": 0}})
        self.assertEqual(status, 201)
        self.call(port, "PUT", "/_mock/v1/scopes/cli-run/settings", {"auth_password": PASSWORD})
        msg = self.message(subject="Your code", body=f"Your PIN is {PIN}\n")
        msg["X-Pin"] = PIN
        msg["X-Api-Token"] = TOKEN
        with smtplib.SMTP(HOST, scope["listener"]["port"], timeout=WAIT_SECONDS) as smtp:
            smtp.ehlo()
            smtp.login(USER, PASSWORD)
            smtp.send_message(msg)
        with smtplib.SMTP(HOST, scope["listener"]["port"], timeout=WAIT_SECONDS) as refused:
            refused.ehlo()
            with self.assertRaises(smtplib.SMTPAuthenticationError):
                refused.login(USER, WRONG_PASSWORD)
        client = imaplib.IMAP4(HOST, scope["imap"]["port"], timeout=WAIT_SECONDS)
        client.login(USER, PASSWORD)
        client.select("INBOX")
        typ, data = client.search(None, "HEADER", "Message-ID", f'"{msg["Message-ID"]}"')
        client.logout()
        self.assertEqual(data, [b"1"])
        self.assertEqual(self.call(port, "DELETE", "/_mock/v1/scopes/cli-run")[0], 204)
        proc.send_signal(signal.SIGTERM)
        out, err = proc.communicate(timeout=EXIT_SECONDS)
        self.assertEqual(proc.returncode, 0)
        self.assertIn("AUTH PLAIN [REDACTED]", err)
        self.assertEqual(leaks(out + err), [], "a secret reached stdout or stderr")
        self.assertEqual(leaks("".join(streamed)), [], "a secret reached the event stream")
        self.assertIn("message.delivered", "".join(streamed))

    def test_manifest_subcommand_prints_the_manifest_file(self) -> None:
        out = subprocess.run(
            [sys.executable, "-m", "mail_mock", "manifest"], cwd=PACKAGE_ROOT, capture_output=True, text=True, timeout=EXIT_SECONDS, check=True
        ).stdout
        self.assertEqual(json.loads(out), json.loads((PACKAGE_ROOT / "mocks.manifest.json").read_text(encoding="utf-8")))

    def test_serve_without_control_is_refused_with_usage(self) -> None:
        proc = subprocess.run([sys.executable, "-m", "mail_mock", "serve"], cwd=PACKAGE_ROOT, capture_output=True, text=True, timeout=EXIT_SECONDS)
        self.assertEqual(proc.returncode, 2)
        self.assertIn("--control", proc.stderr)

    def test_serve_with_a_control_that_is_not_host_port_is_refused_with_usage(self) -> None:
        proc = subprocess.run(
            [sys.executable, "-m", "mail_mock", "serve", "--control", "nonsense"], cwd=PACKAGE_ROOT, capture_output=True, text=True, timeout=EXIT_SECONDS
        )
        self.assertEqual(proc.returncode, 2)
        self.assertIn("host:port", proc.stderr)


if __name__ == "__main__":
    unittest.main()
