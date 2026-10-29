# Mock adapter specification, version 1 (`mocks.adapter/v1`)

This document tells you how to write a **mock** — a program that stands in for a real system — in any
language, so that a Mocks Studio core can run it, scope it to a test run, set its scenarios and read what
it saw. A mock is accepted when the conformance suite in `conformance/` passes against it. There is no
library to link: you implement this specification, you run the suite, you read its lines.

The words MUST, MUST NOT, SHOULD and MAY are used in their RFC 2119 sense.

## 1. The model in one page

- A **run** is one scoped piece of work: a test run, a development session, a person at a console. Its
  name is the **`run_id`**, which matches `^[a-z0-9][a-z0-9._-]{2,63}$`. There is no second id for a scope:
  a scope exists only for a run.
- A **scope** is what your mock keeps for one run: one **listener** (a protocol, a host and a port that the
  core assigns), one set of **settings**, and the events of that run.
- **The listener is the run.** A call that arrives on run A's listener belongs to run A, whatever it
  carries. This is the only carrier that works for protocols with no place for metadata (raw TCP, SMTP,
  DNS), so your mock MUST NOT depend on a header, a tag or a prefix to know the run.
- A **transaction** is one logical exchange; its **`correlation_id`** is a W3C Trace Context trace-id
  (32 lowercase hex characters, not all zero).
- An **event** is one JSON object in the envelope `mocks.event/v1` (`spec/ecosystem-v1/mocks-event-v1.schema.json`).
  Your mock emits an event for everything it receives, sends and changes.
- A **setting** is a named value your mock declares. Failure injection (a forced error, a delay, a failure
  rate) is a setting. It is never read from an environment variable at start.
- A **control surface** is a small HTTP API your mock serves on its **control endpoint**. Only the core
  calls it. Tests, legs and people never do; they call the core, which validates and forwards.

Everything the core does behind this contract (allocating ports, storing events, matching expectations,
issuing run credentials) is the core's business and is not specified here.

## 2. The manifest

Every mock publishes one manifest, `mocks.manifest.json`, a JSON object validated by
`spec/schemas/mocks-manifest-v1.schema.json` (JSON Schema 2020-12).

| field | required | meaning |
|---|---|---|
| `contract` | yes | `mocks.adapter/v1`. A version this specification does not list is refused **by name**, with the supported list (§8). |
| `name` | yes | `^[a-z0-9][a-z0-9-]{1,62}$`; the same value in every event's `mock.name`. |
| `version` | yes | your mock's own version, 1–64 characters; the same value in every event's `mock.version`. |
| `description` | no | one line for a person. |
| `protocols` | yes | a non-empty set drawn from `http`, `tcp`, `smtp`, `dns`, `other`. A scope's listener speaks exactly one of them. |
| `control.url` | yes | the base URL of the control endpoint: scheme, host, port, no path (`http://127.0.0.1:9100`). |
| `settings` | no | the declared settings (§2.1). |
| `safe_fields` | yes | the payload fields your mock may export in clear (§7). May be empty. |
| `correlation_keys` | no | the protocol-native fields that correlate a call without headers: a recipient number, a message reference, a mail `Message-ID`. Names only. |
| `event_kinds` | no | your own event kinds, `x-<name>.<kind>`; an event of an `x-` kind the manifest does not declare is a defect. |
| `callouts` | no | calls your mock makes on its own (an activation push): `name`, and `target_setting`, the **string** setting that carries the URL it is sent to. |
| `conformance` | when `http` or `tcp` is declared | how the suite exercises your listener (§2.2). |
| `x-*` | no | extension fields; ignored by the suite. |

### 2.1 Settings

```json
{ "name": "status_code", "type": "integer", "default": 200, "minimum": 100, "maximum": 599,
  "scope": "run",
  "effect": { "protocol": "http", "value": 503, "expect": { "http_status": 503 } } }
```

- `type` is `string`, `integer`, `number` or `boolean`; `default` is a value of that type.
- `allowed` lists the permitted values; `minimum` and `maximum` bound a number. The default MUST satisfy
  them. A value outside them is refused (§4.5).
- `scope` is `run` (the default: each run has its own value) or `mock` (one value for every run). `reset`
  (§4.4) returns **per-run** settings to their defaults and leaves per-mock settings alone.
- `effect` is a conformance hint: setting `value` makes the next probe on a listener of `protocol` observably
  different from the default — an HTTP status, or the reply line of a TCP probe. The suite uses it to prove
  that a setting changes the next response. A mock whose settings declare no `effect` for a protocol cannot
  be shown to honour them for that protocol, and the suite says so with a `SKIP`.

### 2.2 The probe

```json
"conformance": { "probe": { "http": { "method": "POST", "path": "/probe", "body": "hello" },
                            "tcp":  { "send": "ping" } } }
```

The probe is one request the suite sends to a listener of that protocol to make the mock record an event: an
HTTP request (`method`, `path`, optional `body`), or one text line (`send`; the suite appends a line feed and
reads one line back). It is a real, harmless request of your own protocol; it is not a command.

### 2.3 Discovery

A core finds a mock by one of two equivalent routes:

1. **A manifest file** named `mocks.manifest.json` at the root of the mock's directory or image, and served
   at `GET /_mock/v1/manifest` (§4.2). The two MUST be the same document.
2. **Container labels** under the prefix `mocks.studio/`, carrying the same fields: `mocks.studio/contract`,
   `mocks.studio/name`, `mocks.studio/version`, `mocks.studio/protocols` (comma-separated),
   `mocks.studio/control-url`, `mocks.studio/safe-fields` (comma-separated),
   `mocks.studio/correlation-keys` (comma-separated), and, for the structured fields, a JSON value in
   `mocks.studio/settings`, `mocks.studio/event-kinds`, `mocks.studio/callouts`, `mocks.studio/conformance`.
   A single label `mocks.studio/manifest` holding the whole manifest as JSON is equivalent to all of them.

The suite checks the manifest file or the served manifest. It does not read labels (it needs no container
runtime); a mock that publishes labels is responsible for their agreeing with its manifest.

## 3. Listeners and events

When a scope opens, the core hands your mock a listener: `{protocol, host, port}`. Your mock MUST serve that
run on that listener and nowhere else, MUST answer on it before it answers the open request, and MUST stop
answering on it (close the socket) before it answers the close request.

For every call it receives, your mock MUST emit events in the envelope of
`spec/ecosystem-v1/mocks-event-v1.schema.json`, validated by the suite. In particular:

- `run_id` is the scope's run, from the listener. `mock` is `{name, version, contract}` as in the manifest.
- `at` is UTC with millisecond precision; `event_id` is unique per event.
- A call is recorded as `request.received` (`direction: inbound`); a reply as `response.sent` (`outbound`);
  something your mock delivers out of band as `message.delivered`; a call-out as `callout.sent`; a failure it
  injects as `fault.injected`. A response affected by a setting carries `outcome: fault` and `fault: <setting>`.
- A scope emits `scope.opened` when opened, `scope.reset` when reset, `scope.closed` when closed, and
  `scenario.set` when settings are set.

### 3.1 Correlation

There are three carriers, in this order of authority:

1. **The listener** is authoritative for `run_id`, always.
2. **HTTP headers**, for `correlation_id`: `traceparent` (`00-<trace-id>-<span-id>-<flags>`) carries the
   correlation id as its trace-id; the event records `correlation_source: traceparent`. With no valid
   `traceparent`, your mock generates a valid id (fresh for each transaction) and records
   `correlation_source: generated`. `baggage` may carry `mocks.run=<run_id>` and `mocks.step=<step_id>`.
   No private header is defined.
3. **Protocol-native keys**, declared in `correlation_keys`: the event's `correlation_keys` object carries
   them and `correlation_source` is `correlation-key`.

**A conflict is recorded, never resolved in silence.** A call on run A's listener whose `baggage` says
`mocks.run=B` is still served, and is recorded under A as a `scope.mismatch` event whose summary names both
runs.

## 4. The control surface

All paths are under `/_mock/v1` on `control.url`. Bodies are JSON; requests with a body carry
`Content-Type: application/json`. Only the core calls this surface, and a mock MAY bind it to a private
address.

### 4.1 `GET /_mock/v1/health`

Answers `200` with a JSON object, e.g. `{"status":"ok"}`, when the mock can serve scopes.

### 4.2 `GET /_mock/v1/manifest`

Answers `200` with the manifest (§2), the same document as the manifest file.

### 4.3 `POST /_mock/v1/scopes` — open a scope

Request: `{"run_id": "<run id>", "listener": {"protocol": "http", "host": "127.0.0.1", "port": 18080}}`.
Answers `201` with `{"run_id": "...", "listener": {...}}`. Refusals (§5):

| status | `reason_code` | when |
|---|---|---|
| 422 | `run-id-invalid` | `run_id` does not match the pattern |
| 409 | `run-already-open` | a scope for that run exists; it is never merged or replaced |
| 422 | `listener-protocol-unsupported` | the protocol is not in `protocols`; `allowed` lists those that are |
| 409 | `listener-unavailable` | the listener could not be opened (the port is taken) |

`DELETE /_mock/v1/scopes/{run_id}` closes the scope: it answers `204` after the listener has stopped
answering, and emits `scope.closed`. An unknown run is `404 run-unknown`.

### 4.4 `POST /_mock/v1/scopes/{run_id}/reset`

Answers `204`. Returns the run's per-run settings to their defaults and emits `scope.reset`. An unknown run is
`404 run-unknown`.

### 4.5 `PUT /_mock/v1/scopes/{run_id}/settings`

Request: a JSON object of setting name → value. Answers `200` with `{"settings": {<every setting of the
run, with its current value>}}` and emits `scenario.set`. The request is all-or-nothing: if any entry is
refused, none is applied.

| status | `reason_code` | when |
|---|---|---|
| 422 | `setting-unknown` | a name the manifest does not declare; `allowed` lists the declared names |
| 422 | `setting-type-invalid` | a value of the wrong type |
| 422 | `setting-out-of-range` | a value outside `minimum`/`maximum`, or outside `allowed`; `allowed` lists the permitted values where a list exists |
| 404 | `run-unknown` | no such scope |

### 4.6 `GET /_mock/v1/events` — the event stream

A server-sent event stream (`Content-Type: text/event-stream`), kept open. Each event is one message whose
`data:` is one `mocks.event/v1` JSON object on a single line. `?run_id=<id>` delivers only that run's events.
The stream is live: it carries events from the moment of connection, and a mock is not required to replay
history. A mock SHOULD send a comment line (`: keep-alive`) while idle. The core connects once and keeps the
stream open.

## 5. Refusals

Every refusal is the **Refusal** document of `spec/ecosystem-v1/mocks-runs-v1.openapi.yaml`
(`components.schemas.Refusal`; restated for validation in `spec/schemas/mocks-refusal-v1.schema.json`):

```json
{ "message": "The setting \"foo\" is not declared by this mock and the whole request was refused; use one of the allowed settings.",
  "reason_code": "setting-unknown",
  "allowed": ["status_code"] }
```

- `message`: the **first sentence** says what was measured, what was refused and what to do next.
- `reason_code`: the public key, lower-case words joined by hyphens. Integrations key on it.
- `allowed`: the permitted values, where a list exists.

An HTTP status with a plain-text body, or a refusal without a `reason_code`, is not a refusal and fails the suite.

## 6. What a mock must never do

- Put a credential, token, key or run credential into `summary` or `payload` — in clear, redacted-in-name
  only, or encoded. The suite sends an `Authorization: Bearer …` header and fails the mock if the token appears
  anywhere in any event.
- Export a field that is not in `safe_fields` in clear (§7).
- Read the run from anything but the listener.
- Take a failure from an environment flag at start instead of a declared setting.
- Merge, replace or share state between two runs.

## 7. Payload and safe fields

An event's `payload` is one of:

- `{"redacted": true, "digest": "sha256:<64 hex>", "size": <bytes>}` — nothing of the content;
- `{"redacted": false, "fields": {…}}` — **only** fields named in the manifest's `safe_fields`.

Mock traffic is synthetic, so a mock may declare fields safe in its manifest (an SMS mock's recipient and
text). A field not declared safe is never exported in clear. The core matches expectations against the full
payload on its side; what reaches an agent is a met / not-met answer and the safe fields only.

## 8. Versioning

A new optional field is a minor change and keeps `v1`. A new required field, a removed field or a changed
meaning is `v2`. A runner or core that reads a `contract` it does not list refuses the mock **by name** and
prints the versions it supports; it never guesses. The `contract` field of the manifest schema accepts any
`mocks.adapter/v<N>` for exactly that reason: an unknown version is refused as unknown, not as malformed.

## 9. The conformance suite

`conformance/` is a Go module. Its command `mocks-conformance` checks a running mock:

```
go -C conformance run ./cmd/mocks-conformance --manifest path/to/mocks.manifest.json
go -C conformance run ./cmd/mocks-conformance --control http://127.0.0.1:9100
```

It prints one line per check, `PASS <check-id> — <what was measured>`, `FAIL <check-id> — <what was
measured>` or `SKIP <check-id> — <reason>`, and a `SUMMARY` line. Lines starting `remark:` are limits the
reader should know. A check that runs per protocol carries `/<protocol>` after its id.

**Exit codes**

| code | meaning |
|---|---|
| 0 | every check passed (a `SKIP` is not a failure, and is not a pass) |
| 1 | a check failed |
| 2 | the mock was refused before checking: unknown `contract` version, manifest invalid, nothing to check |
| 3 | the mock could not be reached |

Flags: `--manifest`, `--control` (overrides the manifest's `control.url`, or supplies the manifest when none is
given), `--listen-host` (where the mock is asked to open listeners; default `127.0.0.1`), `--timeout` (bound on
each request), `--event-wait` (how long an expected event is waited for).

**The checks**

| check id | what it measures |
|---|---|
| `contract-known` | `contract` is a version the suite supports; otherwise exit 2, naming the supported versions |
| `manifest-valid` | the manifest validates against its schema and is consistent (unique names, defaults inside their range, `target_setting` naming a string setting, …); otherwise exit 2 |
| `control-reachable` | the control endpoint answers at all; otherwise exit 3 |
| `health-answers` | `GET /health` answers 200 |
| `control-manifest-served` | `GET /manifest` answers 200 and, with `--manifest`, equals the file |
| `events-stream-opens` | `GET /events` answers 200 as `text/event-stream` |
| `scope-opens` | a scope opens (201) and echoes the run and listener |
| `listener-answers` | the listener the core assigned answers a probe |
| `scope-rejects-invalid-run-id` | `run_id` `A1` is refused `422 run-id-invalid` |
| `scope-rejects-duplicate` | a second open of an open run is refused `409 run-already-open`, and the first scope keeps answering |
| `scope-rejects-unsupported-protocol` | a listener of an undeclared protocol is refused `422 listener-protocol-unsupported` with `allowed` |
| `event-valid` | a call on the listener emits events that validate against `mocks.event/v1`, carry the scope's `run_id` and the manifest's `mock`, and use only declared custom kinds |
| `scope-lifecycle-events` | `scope.opened` and `scope.closed` arrive |
| `correlation-traceparent` | a `traceparent` sent becomes the event's `correlation_id`, with `correlation_source: traceparent` (http) |
| `correlation-generated` | with none sent, a valid id is generated, labelled `generated`, and differs between calls |
| `scope-mismatch-recorded` | a call whose `baggage` names another run is served and recorded as `scope.mismatch` under the listener's run (http) |
| `scopes-isolated` | two open scopes keep their events apart |
| `events-filter-by-run` | `?run_id=` delivers that run's events and no other |
| `setting-unknown-refused` | an unknown setting is refused `422 setting-unknown` with the declared names as `allowed` |
| `setting-out-of-range-refused` | an out-of-range or not-allowed value is refused `422 setting-out-of-range` |
| `setting-type-refused` | a value of the wrong type is refused `422 setting-type-invalid` |
| `setting-changes-response` | a declared setting, accepted, changes the next response as its `effect` says |
| `scenario-set-event` | setting a setting emits a valid `scenario.set` |
| `reset-restores-defaults` | reset returns the setting to its default behaviour |
| `scope-reset-event` | reset emits a valid `scope.reset` |
| `payload-safe-fields-only` | no non-safe field is exported in clear, and the probe's content is not repeated outside `payload.fields` |
| `no-credential-in-event` | a credential sent in `Authorization` appears in no event (http) |
| `listener-closed-after-delete` | after close, the listener no longer answers |

A check whose prerequisite failed is reported `SKIP`, so that a mock which breaks one rule fails that rule alone.
A check that does not apply to the mock's declared shape (no `traceparent` on a raw TCP listener, no range to
exceed) is a `SKIP` with its reason.

**Limits of what the suite proves** (so you do not read more into a green run than it measured):

- It drives `http` and `tcp` listeners. A mock that declares only other protocols is refused (exit 2): the
  suite cannot check what it cannot drive.
- The digest of a redacted payload (`sha256:…`) is checked for its form only, not for what it is a digest of.
- `fault.injected`, call-outs and `correlation-key` correlation are validated for their envelope when they
  occur, but the suite does not provoke them.
- Per-mock settings, concurrency under load and event ordering across listeners are not exercised.
- A connection refused and a connection the host forbids look the same to a runner: `listener-closed-after-delete`
  and exit 3 read an inability to connect as "closed" and "unreachable". Run the suite where loopback networking
  is permitted.

## 10. Files

| path | what |
|---|---|
| `spec/MOCK_ADAPTER_V1.md` | this specification |
| `spec/schemas/mocks-manifest-v1.schema.json` | the manifest schema |
| `spec/schemas/mocks-refusal-v1.schema.json` | the refusal schema |
| `spec/ecosystem-v1/mocks-event-v1.schema.json` | the event envelope (shared by Mocks Studio, Tests Studio and Saphan Studio) |
| `spec/ecosystem-v1/mocks-event-v1.examples.json` | valid and invalid events, each invalid one breaking one field |
| `spec/ecosystem-v1/mocks-runs-v1.openapi.yaml` | the run and scenario operations on the core (OpenAPI 3.1) |
| `conformance/` | the suite and its fixtures: a null mock and, for each rule, a mutant that breaks only that rule |

### A note for implementers whose regular-expression engine is not ECMAScript

`mocks-event-v1.schema.json` uses only RE2-clean regular expressions: the trace-id rule is `^[0-9a-f]{32}$`
together with a `not` of the constant of 32 zeros, so an RE2-family engine (Go, Rust's `regex`, …) compiles the
published file as it is.
