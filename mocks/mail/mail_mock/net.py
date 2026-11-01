"""A threaded TCP listener that can be stopped completely: the socket and every open connection."""

from __future__ import annotations

import socket
import socketserver
import threading
from collections.abc import Callable

#: How often the accept loop looks at its stop flag, in seconds.
ACCEPT_POLL_SECONDS = 0.05

#: Idle time after which a session's read gives up, in seconds.
SESSION_IDLE_SECONDS = 60.0


class _Server(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True
    block_on_close = False

    def __init__(self, address: tuple[str, int], handler: Callable[[socket.socket, tuple], None]) -> None:
        self._session = handler
        self._conns: set[socket.socket] = set()
        self._conns_lock = threading.Lock()
        super().__init__(address, _Handler)

    def process_request(self, request, client_address):  # type: ignore[no-untyped-def]
        with self._conns_lock:
            self._conns.add(request)
        super().process_request(request, client_address)

    def forget(self, conn: socket.socket) -> None:
        with self._conns_lock:
            self._conns.discard(conn)

    def drop_connections(self) -> None:
        with self._conns_lock:
            conns = list(self._conns)
        for conn in conns:
            try:
                conn.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass  # already closed by the peer or its session


class _Handler(socketserver.BaseRequestHandler):
    server: _Server

    def handle(self) -> None:
        conn: socket.socket = self.request
        try:
            conn.settimeout(SESSION_IDLE_SECONDS)
            self.server._session(conn, self.client_address)
        finally:
            self.server.forget(conn)


class Listener:
    """Binds at construction (so a taken port is an error there) and serves until `stop`."""

    def __init__(self, host: str, port: int, session: Callable[[socket.socket, tuple], None]) -> None:
        self._server = _Server((host, port), session)
        self.host, self.port = self._server.server_address[:2]
        self._thread = threading.Thread(
            target=self._server.serve_forever, kwargs={"poll_interval": ACCEPT_POLL_SECONDS}, daemon=True
        )
        self._thread.start()

    def track(self, conn: socket.socket) -> None:
        """Count a connection the session made from the accepted one (a TLS wrap) among those `stop` cuts."""
        with self._server._conns_lock:
            self._server._conns.add(conn)

    def stop(self) -> None:
        """Stop accepting, close the socket, and cut every open connection."""
        self._server.shutdown()
        self._server.server_close()
        self._server.drop_connections()
        self._thread.join(timeout=2.0)
