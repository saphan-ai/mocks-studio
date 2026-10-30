"""The control surface (`/_mock/v1`, spec section 4) over HTTP."""

from __future__ import annotations

import json
import logging
import queue
import re
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import TYPE_CHECKING, Any
from urllib.parse import parse_qs, urlsplit

from . import manifest
from .scope import Refusal

if TYPE_CHECKING:
    from .app import MailMock

log = logging.getLogger("mail_mock")

BASE = "/_mock/v1"
#: Largest control request body accepted, in bytes.
MAX_BODY_BYTES = 64 * 1024
#: Seconds of silence on the event stream before a keep-alive comment is sent.
KEEPALIVE_SECONDS = 5.0
#: How often the stream loop checks whether the server is stopping, in seconds.
STREAM_POLL_SECONDS = 0.25

_SCOPE_PATH = re.compile(rf"^{BASE}/scopes/(?P<run>[^/]+)(?P<tail>/reset|/settings)?$")


class ControlServer:
    """The HTTP server of the control endpoint."""

    def __init__(self, mock: "MailMock", host: str, port: int) -> None:
        self.mock = mock
        self.stopping = threading.Event()
        handler = type("Handler", (_Handler,), {"mock": mock, "stopping": self.stopping, "control_url": ""})
        self._httpd = ThreadingHTTPServer((host, port), handler)
        self._httpd.daemon_threads = True
        self.host, self.port = self._httpd.server_address[:2]
        handler.control_url = f"http://{self.host}:{self.port}"
        self.url = handler.control_url
        self._thread = threading.Thread(target=self._httpd.serve_forever, kwargs={"poll_interval": 0.05}, daemon=True)
        self._thread.start()

    def stop(self) -> None:
        self.stopping.set()
        self._httpd.shutdown()
        self._httpd.server_close()
        self._thread.join(timeout=2.0)


class _Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    mock: "MailMock"
    stopping: threading.Event
    control_url: str

    def log_message(self, format: str, *args: Any) -> None:  # noqa: A002 - signature fixed by the base class
        log.debug("control %s %s", self.command, urlsplit(self.path).path)  # the path only: never a body

    # -- routing ----------------------------------------------------------------------------

    def do_GET(self) -> None:
        self._route("GET")

    def do_POST(self) -> None:
        self._route("POST")

    def do_PUT(self) -> None:
        self._route("PUT")

    def do_DELETE(self) -> None:
        self._route("DELETE")

    def _route(self, method: str) -> None:
        url = urlsplit(self.path)
        try:
            if url.path == f"{BASE}/health" and method == "GET":
                self._json(200, {"status": "ok"})
            elif url.path == f"{BASE}/manifest" and method == "GET":
                self._json(200, manifest.build(self.control_url))
            elif url.path == f"{BASE}/events" and method == "GET":
                self._stream(parse_qs(url.query).get("run_id", [None])[0])
            elif url.path == f"{BASE}/scopes" and method == "POST":
                self._json(201, self.mock.open_scope(self._body()).description())
            else:
                self._scope_route(method, url.path)
        except Refusal as refusal:
            self._json(refusal.status, refusal.document())

    def _scope_route(self, method: str, path: str) -> None:
        m = _SCOPE_PATH.match(path)
        if m is None:
            raise Refusal(404, "not-found", f"The path {path} is not part of the control surface and nothing was done; see spec section 4 for the paths.")
        run, tail = m.group("run"), m.group("tail")
        if tail is None and method == "DELETE":
            self.mock.close_scope(run)
            self._empty(204)
        elif tail == "/reset" and method == "POST":
            self.mock.reset(run)
            self._empty(204)
        elif tail == "/settings" and method == "PUT":
            self._json(200, {"settings": self.mock.put_settings(run, self._body())})
        else:
            raise Refusal(405, "method-not-allowed", f"{method} is not allowed on {path} and nothing was done; use the method the spec lists for this path.")

    # -- bodies -----------------------------------------------------------------------------

    def _body(self) -> Any:
        length = int(self.headers.get("Content-Length") or 0)
        if length > MAX_BODY_BYTES:
            raise Refusal(413, "request-too-large", f"The request body is {length} bytes and was refused; send at most {MAX_BODY_BYTES} bytes.")
        raw = self.rfile.read(length) if length else b""
        try:
            return json.loads(raw) if raw else None
        except ValueError as exc:
            raise Refusal(400, "request-invalid", "The request body is not valid JSON and nothing was done; send a JSON object.") from exc

    def _json(self, status: int, doc: Any) -> None:
        data = json.dumps(doc).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def _empty(self, status: int) -> None:
        self.send_response(status)
        self.send_header("Content-Length", "0")
        self.end_headers()

    # -- the event stream -------------------------------------------------------------------

    def _stream(self, run_id: str | None) -> None:
        sub = self.mock.bus.subscribe(run_id)
        self.close_connection = True
        try:
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-cache")
            self.send_header("Connection", "close")
            self.end_headers()
            self.wfile.write(b": connected\n\n")
            self.wfile.flush()
            self._pump(sub)
        except (BrokenPipeError, ConnectionResetError):
            pass  # the reader went away; the subscription is dropped below
        finally:
            self.mock.bus.unsubscribe(sub)

    def _pump(self, sub: Any) -> None:
        idle = 0.0
        while not self.stopping.is_set():
            try:
                line = sub.queue.get(timeout=STREAM_POLL_SECONDS)
            except queue.Empty:
                idle += STREAM_POLL_SECONDS
                if idle >= KEEPALIVE_SECONDS:
                    self.wfile.write(b": keep-alive\n\n")
                    self.wfile.flush()
                    idle = 0.0
                continue
            idle = 0.0
            self.wfile.write(b"data: " + line.encode("utf-8") + b"\n\n")
            self.wfile.flush()
