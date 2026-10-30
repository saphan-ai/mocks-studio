"""The control surface, the tcp probe, and the lifecycle of a scope's listeners."""

from __future__ import annotations

import http.client
import json
import socket
import threading
from pathlib import Path
from typing import Any

from mail_mock import manifest

from .support import HOST, WAIT_SECONDS, MockTestCase, RawSmtp, assert_valid_event

MANIFEST_FILE = Path(__file__).resolve().parents[1] / "mocks.manifest.json"


class ControlTestCase(MockTestCase):
    def setUp(self) -> None:
        super().setUp()
        self.control = self.mock.serve_control(HOST, 0)

    def request(self, method: str, path: str, body: Any = None) -> tuple[int, Any]:
        conn = http.client.HTTPConnection(HOST, self.control.port, timeout=WAIT_SECONDS)
        try:
            data = None if body is None else json.dumps(body)
            conn.request(method, path, data, {"Content-Type": "application/json"} if data else {})
            resp = conn.getresponse()
            raw = resp.read()
            return resp.status, (json.loads(raw) if raw else None)
        finally:
            conn.close()

    def open_http(self, run_id: str, protocol: str = "smtp", port: int = 0) -> dict[str, Any]:
        status, doc = self.request("POST", "/_mock/v1/scopes", {"run_id": run_id, "listener": {"protocol": protocol, "host": HOST, "port": port}})
        self.assertEqual(status, 201, doc)
        return doc

    def assert_refusal(self, got: tuple[int, Any], status: int, code: str) -> dict[str, Any]:
        self.assertEqual(got[0], status, got[1])
        self.assertEqual(got[1]["reason_code"], code)
        self.assertRegex(got[1]["message"], r"^[A-Z].*[.;]", "the first sentence says what happened and what to do")
        return got[1]


class ManifestTest(ControlTestCase):
    def test_the_served_manifest_is_the_manifest_file_but_for_the_real_control_url(self) -> None:
        status, served = self.request("GET", "/_mock/v1/manifest")
        on_file = json.loads(MANIFEST_FILE.read_text(encoding="utf-8"))
        self.assertEqual(status, 200)
        self.assertEqual(served["control"]["url"], f"http://{HOST}:{self.control.port}")
        served["control"]["url"] = on_file["control"]["url"]
        self.assertEqual(served, on_file)

    def test_the_manifest_file_is_what_the_code_renders(self) -> None:
        self.assertEqual(json.loads(MANIFEST_FILE.read_text(encoding="utf-8")), manifest.build())

    def test_the_manifest_declares_no_secret_in_safe_fields(self) -> None:
        self.assertEqual(set(manifest.SAFE_FIELDS), {"from", "to", "subject", "message_id"})

    def test_every_declared_setting_has_a_tcp_effect_except_the_credentials(self) -> None:
        without = {s["name"] for s in manifest.settings() if "effect" not in s}
        self.assertEqual(without, {manifest.AUTH_USER, manifest.AUTH_PASSWORD})

    def test_health_answers_200(self) -> None:
        self.assertEqual(self.request("GET", "/_mock/v1/health"), (200, {"status": "ok"}))


class ScopeControlTest(ControlTestCase):
    def test_open_answers_201_with_the_listener_the_imap_side_and_a_certificate(self) -> None:
        doc = self.open_http("ctl-open")
        self.assertEqual(doc["run_id"], "ctl-open")
        self.assertEqual(doc["listener"]["protocol"], "smtp")
        self.assertGreater(doc["listener"]["port"], 0)
        self.assertGreater(doc["imap"]["port"], 0)
        self.assertTrue(doc["tls_certificate_pem"].startswith("-----BEGIN CERTIFICATE-----"))

    def test_open_with_run_id_a1_is_refused_422_run_id_invalid(self) -> None:
        body = {"run_id": "A1", "listener": {"protocol": "smtp", "host": HOST, "port": 0}}
        self.assert_refusal(self.request("POST", "/_mock/v1/scopes", body), 422, "run-id-invalid")

    def test_open_of_an_open_run_is_refused_409_and_the_first_scope_keeps_answering(self) -> None:
        doc = self.open_http("ctl-dup")
        body = {"run_id": "ctl-dup", "listener": {"protocol": "smtp", "host": HOST, "port": 0}}
        self.assert_refusal(self.request("POST", "/_mock/v1/scopes", body), 409, "run-already-open")
        with RawSmtp(doc["listener"]["port"]) as raw:
            self.assertEqual(raw.greeting[0], 220)

    def test_open_with_an_undeclared_protocol_is_refused_422_with_the_allowed_protocols(self) -> None:
        body = {"run_id": "ctl-http", "listener": {"protocol": "http", "host": HOST, "port": 0}}
        refusal = self.assert_refusal(self.request("POST", "/_mock/v1/scopes", body), 422, "listener-protocol-unsupported")
        self.assertEqual(sorted(refusal["allowed"]), ["smtp", "tcp"])

    def test_open_on_a_port_that_is_taken_is_refused_409_and_leaves_no_scope(self) -> None:
        with socket.socket() as taken:
            taken.bind((HOST, 0))
            taken.listen()
            body = {"run_id": "ctl-taken", "listener": {"protocol": "smtp", "host": HOST, "port": taken.getsockname()[1]}}
            self.assert_refusal(self.request("POST", "/_mock/v1/scopes", body), 409, "listener-unavailable")
        self.open_http("ctl-taken")  # the run id is free again

    def test_open_without_a_listener_is_refused_422(self) -> None:
        self.assert_refusal(self.request("POST", "/_mock/v1/scopes", {"run_id": "ctl-nolistener"}), 422, "listener-invalid")

    def test_open_with_a_body_that_is_not_json_is_refused_400(self) -> None:
        conn = http.client.HTTPConnection(HOST, self.control.port, timeout=WAIT_SECONDS)
        conn.request("POST", "/_mock/v1/scopes", "{not json", {"Content-Type": "application/json"})
        resp = conn.getresponse()
        self.assertEqual((resp.status, json.loads(resp.read())["reason_code"]), (400, "request-invalid"))
        conn.close()

    def test_delete_of_an_unknown_run_is_404_run_unknown(self) -> None:
        self.assert_refusal(self.request("DELETE", "/_mock/v1/scopes/never-opened"), 404, "run-unknown")

    def test_reset_of_an_unknown_run_is_404_run_unknown(self) -> None:
        self.assert_refusal(self.request("POST", "/_mock/v1/scopes/never-opened/reset"), 404, "run-unknown")

    def test_settings_of_an_unknown_run_is_404_run_unknown(self) -> None:
        self.assert_refusal(self.request("PUT", "/_mock/v1/scopes/never-opened/settings", {manifest.TEMPFAIL: True}), 404, "run-unknown")

    def test_an_unknown_path_is_a_404_refusal_document(self) -> None:
        self.assert_refusal(self.request("GET", "/_mock/v1/nothing-here"), 404, "not-found")

    def test_a_wrong_method_on_a_scope_path_is_a_405_refusal_document(self) -> None:
        self.open_http("ctl-method")
        self.assert_refusal(self.request("GET", "/_mock/v1/scopes/ctl-method/settings"), 405, "method-not-allowed")

    def test_delete_stops_both_listeners_before_it_answers_204(self) -> None:
        doc = self.open_http("ctl-close")
        smtp_port, imap_port = doc["listener"]["port"], doc["imap"]["port"]
        with RawSmtp(smtp_port):
            pass
        self.assertEqual(self.request("DELETE", "/_mock/v1/scopes/ctl-close")[0], 204)
        for port in (smtp_port, imap_port):
            with self.assertRaises(OSError):
                socket.create_connection((HOST, port), timeout=1.0).close()
        self.assertEqual([e["kind"] for e in self.tap.drain()][-1], "scope.closed")

    def test_delete_cuts_a_session_that_is_still_open(self) -> None:
        doc = self.open_http("ctl-cut")
        raw = RawSmtp(doc["listener"]["port"])
        self.addCleanup(raw.close)
        self.request("DELETE", "/_mock/v1/scopes/ctl-cut")
        with self.assertRaises((ConnectionError, OSError)):
            raw.cmd("NOOP")


class SettingsControlTest(ControlTestCase):
    def put(self, run: str, body: Any) -> tuple[int, Any]:
        return self.request("PUT", f"/_mock/v1/scopes/{run}/settings", body)

    def test_settings_answers_every_setting_with_its_current_value(self) -> None:
        self.open_http("set-all")
        status, doc = self.put("set-all", {manifest.TEMPFAIL: True})
        self.assertEqual(status, 200)
        self.assertEqual(set(doc["settings"]), {s["name"] for s in manifest.settings()})
        self.assertIs(doc["settings"][manifest.TEMPFAIL], True)
        self.assertEqual(doc["settings"][manifest.SMTP_DELAY_MS], 0)

    def test_an_unknown_setting_is_refused_422_with_the_declared_names(self) -> None:
        self.open_http("set-unknown")
        refusal = self.assert_refusal(self.put("set-unknown", {"no_such": 1}), 422, "setting-unknown")
        self.assertEqual(sorted(refusal["allowed"]), sorted(s["name"] for s in manifest.settings()))

    def test_a_string_for_a_boolean_setting_is_refused_422_type_invalid(self) -> None:
        self.open_http("set-type")
        self.assert_refusal(self.put("set-type", {manifest.TEMPFAIL: "yes"}), 422, "setting-type-invalid")

    def test_true_for_an_integer_setting_is_refused_422_type_invalid(self) -> None:
        self.open_http("set-bool-int")
        self.assert_refusal(self.put("set-bool-int", {manifest.SMTP_DELAY_MS: True}), 422, "setting-type-invalid")

    def test_a_number_for_a_string_setting_is_refused_422_type_invalid(self) -> None:
        self.open_http("set-num-str")
        self.assert_refusal(self.put("set-num-str", {manifest.REFUSE_RECIPIENT: 12345}), 422, "setting-type-invalid")

    def test_a_delay_above_the_maximum_is_refused_422_out_of_range(self) -> None:
        self.open_http("set-range-hi")
        self.assert_refusal(self.put("set-range-hi", {manifest.SMTP_DELAY_MS: manifest.DELAY_MAX_MS + 1}), 422, "setting-out-of-range")

    def test_a_delay_below_the_minimum_is_refused_422_out_of_range(self) -> None:
        self.open_http("set-range-lo")
        self.assert_refusal(self.put("set-range-lo", {manifest.SMTP_DELAY_MS: -1}), 422, "setting-out-of-range")

    def test_a_body_that_is_not_an_object_is_refused_400(self) -> None:
        self.open_http("set-not-object")
        self.assert_refusal(self.put("set-not-object", [1, 2]), 400, "request-invalid")

    def test_one_refused_entry_applies_none_of_the_request(self) -> None:
        self.open_http("set-atomic")
        self.assertEqual(self.put("set-atomic", {manifest.TEMPFAIL: True, manifest.SMTP_DELAY_MS: 99999})[0], 422)
        status, doc = self.put("set-atomic", {})
        self.assertIs(doc["settings"][manifest.TEMPFAIL], False)

    def test_reset_returns_every_per_run_setting_to_its_default_and_emits_scope_reset(self) -> None:
        self.open_http("set-reset")
        self.put("set-reset", {manifest.TEMPFAIL: True, manifest.AUTH_PASSWORD: "changed-fictional-pw"})
        self.assertEqual(self.request("POST", "/_mock/v1/scopes/set-reset/reset")[0], 204)
        status, doc = self.put("set-reset", {})
        self.assertIs(doc["settings"][manifest.TEMPFAIL], False)
        self.assertEqual(doc["settings"][manifest.AUTH_PASSWORD], manifest.DEFAULT_PASSWORD)
        self.assertIn("scope.reset", [e["kind"] for e in self.tap.drain()])

    def test_scenario_set_names_the_settings_and_never_their_values(self) -> None:
        self.open_http("set-event")
        self.put("set-event", {manifest.AUTH_PASSWORD: "value-that-must-not-appear"})
        event = next(e for e in self.tap.drain() if e["kind"] == "scenario.set")
        self.assertIn(manifest.AUTH_PASSWORD, event["summary"])
        self.assertNotIn("value-that-must-not-appear", json.dumps(event))

    def test_settings_of_one_scope_do_not_reach_another(self) -> None:
        self.open_http("set-iso-a")
        self.open_http("set-iso-b")
        self.put("set-iso-a", {manifest.TEMPFAIL: True})
        self.assertIs(self.put("set-iso-b", {})[1]["settings"][manifest.TEMPFAIL], False)


class ProbeTest(ControlTestCase):
    """A `tcp` listener answers a client that speaks first with one status line, per the manifest's effects."""

    def probe(self, port: int, line: str = "ping") -> str:
        with socket.create_connection((HOST, port), timeout=WAIT_SECONDS) as sock:
            sock.sendall(line.encode() + b"\n")
            return sock.makefile("rb").readline().decode().rstrip("\r\n")

    def test_the_default_probe_reply_is_the_ok_line(self) -> None:
        port = self.open_http("probe-default", "tcp")["listener"]["port"]
        self.assertEqual(self.probe(port), manifest.PROBE_REPLY_OK)

    def test_each_setting_changes_the_probe_reply_as_the_manifest_declares(self) -> None:
        for spec in manifest.settings():
            effect = spec.get("effect")
            if effect is None:
                continue
            with self.subTest(setting=spec["name"]):
                run = "probe-" + spec["name"].replace("_", "-")
                port = self.open_http(run, "tcp")["listener"]["port"]
                self.assertNotEqual(self.probe(port), effect["expect"]["tcp_reply"])
                self.assertEqual(self.request("PUT", f"/_mock/v1/scopes/{run}/settings", {spec["name"]: effect["value"]})[0], 200)
                self.assertEqual(self.probe(port), effect["expect"]["tcp_reply"])
                self.request("POST", f"/_mock/v1/scopes/{run}/reset")
                self.assertEqual(self.probe(port), manifest.PROBE_REPLY_OK)

    def test_a_probe_emits_request_received_and_response_sent_with_generated_ids_that_differ(self) -> None:
        port = self.open_http("probe-events", "tcp")["listener"]["port"]
        self.probe(port, "ping marker-1")
        self.probe(port, "ping marker-2")
        self.tap.wait_for(lambda e: e["kind"] == "response.sent" and e["run_id"] == "probe-events", "first response.sent")
        received = [e for e in self.tap.drain() if e["kind"] == "request.received"]
        self.assertEqual([e["correlation_source"] for e in received], ["generated", "generated"])
        self.assertEqual(len({e["correlation_id"] for e in received}), 2)
        self.assertNotIn("marker-1", json.dumps([{k: v for k, v in e.items() if k != "payload"} for e in self.tap.events]))

    def test_a_probe_answered_by_a_setting_emits_fault_injected(self) -> None:
        self.open_http("probe-fault", "tcp")
        self.request("PUT", "/_mock/v1/scopes/probe-fault/settings", {manifest.TEMPFAIL: True})
        self.probe(self.mock.scope("probe-fault").port)
        self.assertEqual(self.tap.wait_for(lambda e: e["kind"] == "fault.injected", "fault.injected")["fault"], manifest.TEMPFAIL)

    def test_a_tcp_scope_still_serves_smtp_to_a_client_that_waits_for_the_greeting(self) -> None:
        port = self.open_http("probe-smtp", "tcp")["listener"]["port"]
        with RawSmtp(port) as raw:
            self.assertEqual(raw.greeting[0], 220)
            self.assertEqual(raw.ehlo()[0], 250)

    def test_an_smtp_scope_greets_at_once_and_never_treats_a_client_as_a_probe(self) -> None:
        port = self.open_http("probe-plain", "smtp")["listener"]["port"]
        with socket.create_connection((HOST, port), timeout=WAIT_SECONDS) as sock:
            sock.sendall(b"ping\r\n")
            lines = sock.makefile("rb")
            self.assertTrue(lines.readline().startswith(b"220 "))
            self.assertTrue(lines.readline().startswith(b"500 "))


class EventStreamTest(ControlTestCase):
    def read_stream(self, path: str, want: int) -> list[dict[str, Any]]:
        """Open the SSE stream, and collect `want` events (or give up after the wait)."""
        got: list[dict[str, Any]] = []
        ready = threading.Event()
        done = threading.Event()

        def pump() -> None:
            conn = http.client.HTTPConnection(HOST, self.control.port, timeout=WAIT_SECONDS)
            conn.request("GET", path)
            resp = conn.getresponse()
            self.assertEqual(resp.getheader("Content-Type"), "text/event-stream")
            ready.set()
            try:
                while len(got) < want:
                    line = resp.readline().decode()
                    if line.startswith("data: "):
                        got.append(json.loads(line[6:]))
            except OSError:
                pass  # the wait elapsed and the socket timed out
            finally:
                conn.close()
                done.set()

        threading.Thread(target=pump, daemon=True).start()
        self.assertTrue(ready.wait(WAIT_SECONDS))
        self.stream_done = done
        return got

    def test_the_stream_delivers_events_as_one_json_object_per_data_line_and_validates(self) -> None:
        got = self.read_stream("/_mock/v1/events", 1)
        self.open_http("sse-basic")
        self.assertTrue(self.stream_done.wait(WAIT_SECONDS))
        assert_valid_event(self, got[0])
        self.assertEqual((got[0]["kind"], got[0]["run_id"]), ("scope.opened", "sse-basic"))

    def test_the_stream_filtered_by_run_id_carries_that_run_and_no_other(self) -> None:
        got = self.read_stream("/_mock/v1/events?run_id=sse-b", 2)
        self.open_http("sse-a")
        self.open_http("sse-b")
        self.request("DELETE", "/_mock/v1/scopes/sse-a")
        self.request("DELETE", "/_mock/v1/scopes/sse-b")
        self.assertTrue(self.stream_done.wait(WAIT_SECONDS))
        self.assertEqual([(e["kind"], e["run_id"]) for e in got], [("scope.opened", "sse-b"), ("scope.closed", "sse-b")])
