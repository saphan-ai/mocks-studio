"""`python -m mail_mock serve --control <host:port>` and `python -m mail_mock manifest`."""

from __future__ import annotations

import argparse
import json
import logging
import signal
import sys
import threading

from . import manifest

DEFAULT_PROBE_GRACE_MS = manifest.DEFAULT_PROBE_GRACE_MS
LOG_FORMAT = "%(asctime)s %(levelname)s %(message)s"


def _split_control(value: str) -> tuple[str, int]:
    """`host:port`, with an optional `http://` in front."""
    value = value.removeprefix("http://").rstrip("/")
    host, sep, port = value.rpartition(":")
    if not sep or not host or not port.isdigit():
        raise argparse.ArgumentTypeError(f"{value!r} is not host:port; write for example 127.0.0.1:19100")
    return host, int(port)


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="mail_mock", description="SMTP relay and IMAP mailbox mock for mocks.adapter/v1")
    sub = parser.add_subparsers(dest="command", required=True)
    serve = sub.add_parser("serve", help="serve the control surface and, per scope, SMTP and IMAP")
    serve.add_argument("--control", required=True, type=_split_control, metavar="HOST:PORT", help="where the control endpoint listens")
    serve.add_argument(
        "--probe-grace-ms", type=int, default=DEFAULT_PROBE_GRACE_MS,
        help="how long a tcp listener waits for a client that speaks first before it greets (default %(default)s)",
    )
    out = sub.add_parser("manifest", help="print the manifest as mocks.manifest.json")
    out.add_argument("--control-url", default=manifest.DEFAULT_CONTROL_URL)
    return parser


def main(argv: list[str] | None = None) -> int:
    args = _parser().parse_args(argv)
    if args.command == "manifest":
        sys.stdout.write(json.dumps(manifest.build(args.control_url), indent=2) + "\n")
        return 0
    logging.basicConfig(level=logging.INFO, format=LOG_FORMAT, stream=sys.stderr)
    try:
        from .app import MailMock
    except ModuleNotFoundError as exc:
        if exc.name != "cryptography":
            raise
        sys.stderr.write("mail_mock needs the 'cryptography' package to make each scope's self-signed certificate; run: pip install -r requirements.txt\n")
        return 2
    mock = MailMock(probe_grace_ms=args.probe_grace_ms)
    host, port = args.control
    control = mock.serve_control(host, port)
    sys.stdout.write(f"mail-mock control listening on {control.url}\n")
    sys.stdout.flush()
    stop = threading.Event()
    for sig in (signal.SIGINT, signal.SIGTERM):
        signal.signal(sig, lambda *_: stop.set())
    stop.wait()
    mock.shutdown()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
