"""The event bus: builds `mocks.event/v1` envelopes and fans them out to stream subscribers."""

from __future__ import annotations

import hashlib
import json
import queue
import threading
import uuid
from datetime import datetime, timezone
from typing import Any

from . import manifest

#: How many events one subscriber may fall behind before it loses the oldest ones.
SUBSCRIBER_BACKLOG = 10000

ZERO_TRACE_ID = "0" * 32


def now_iso() -> str:
    """UTC with millisecond precision, the form the envelope requires."""
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%f")[:-3] + "Z"


def new_correlation_id() -> str:
    """A fresh valid W3C trace-id."""
    while True:
        value = uuid.uuid4().hex
        if value != ZERO_TRACE_ID:
            return value


def correlation_id_for_key(value: str) -> str:
    """The trace-id derived from a protocol-native key, so one Message-ID always maps to one id."""
    digest = hashlib.sha256(value.encode("utf-8")).hexdigest()[:32]
    return digest if digest != ZERO_TRACE_ID else "1" * 32


def redacted_payload(content: bytes | str) -> dict[str, Any]:
    """A payload that exports nothing of `content` but its digest and size."""
    data = content.encode("utf-8") if isinstance(content, str) else content
    return {"redacted": True, "digest": "sha256:" + hashlib.sha256(data).hexdigest(), "size": len(data)}


def clear_payload(fields: dict[str, str]) -> dict[str, Any]:
    """A payload of safe fields only; the caller passes nothing outside `manifest.SAFE_FIELDS`."""
    unsafe = set(fields) - set(manifest.SAFE_FIELDS)
    if unsafe:
        raise ValueError(f"fields {sorted(unsafe)} are not declared safe in the manifest")
    return {"redacted": False, "fields": dict(fields)}


class Subscriber:
    """One open event stream, optionally filtered to one run."""

    def __init__(self, run_id: str | None) -> None:
        self.run_id = run_id
        self.queue: queue.Queue[str] = queue.Queue(maxsize=SUBSCRIBER_BACKLOG)

    def offer(self, line: str) -> None:
        """Queue one event line; a subscriber that is full drops its oldest event."""
        while True:
            try:
                self.queue.put_nowait(line)
                return
            except queue.Full:
                try:
                    self.queue.get_nowait()
                except queue.Empty:
                    pass


class EventBus:
    """Builds events and delivers them to every matching subscriber, in emission order."""

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._subscribers: list[Subscriber] = []

    def subscribe(self, run_id: str | None) -> Subscriber:
        sub = Subscriber(run_id)
        with self._lock:
            self._subscribers.append(sub)
        return sub

    def unsubscribe(self, sub: Subscriber) -> None:
        with self._lock:
            if sub in self._subscribers:
                self._subscribers.remove(sub)

    def emit(
        self,
        run_id: str,
        kind: str,
        direction: str,
        protocol: str,
        summary: str,
        *,
        correlation_id: str | None = None,
        correlation_source: str = "generated",
        correlation_keys: dict[str, str] | None = None,
        outcome: str | None = None,
        fault: str | None = None,
        latency_ms: int | None = None,
        payload: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        """Build one envelope and deliver it; the envelope is returned for tests."""
        event: dict[str, Any] = {
            "schema": "mocks.event/v1",
            "event_id": "evt-" + uuid.uuid4().hex,
            "at": now_iso(),
            "run_id": run_id,
            "correlation_id": correlation_id or new_correlation_id(),
            "correlation_source": correlation_source,
            "mock": {"name": manifest.NAME, "version": manifest.VERSION, "contract": manifest.CONTRACT},
            "kind": kind,
            "direction": direction,
            "protocol": protocol,
            "summary": summary[:512] or "event",
        }
        if correlation_keys:
            event["correlation_keys"] = correlation_keys
        if outcome:
            event["outcome"] = outcome
        if fault:
            event["fault"] = fault
        if latency_ms is not None:
            event["latency_ms"] = latency_ms
        if payload is not None:
            event["payload"] = payload
        line = json.dumps(event, separators=(",", ":"))
        with self._lock:
            targets = [s for s in self._subscribers if s.run_id in (None, run_id)]
            for sub in targets:
                sub.offer(line)
        return event
