---
name: openapi-mock
description: Serve any OpenAPI 3.0 or 3.1 document as a mock of that API that Mocks Studio accepts. Use when you need a stand-in for a REST system that has an OpenAPI document, when you must test against failures (a forced status, a delay, a failure rate), or when you must assert on what a client sent to it.
license: Apache-2.0
metadata:
  saphan.origin: mocks-studio/mocks/openapi
  saphan.contract: mocks.adapter/v1
---

# openapi-mock

One command turns an OpenAPI document into a mock. The mock validates every request against the document,
answers from the document's examples or schemas, records every call as an event, and takes scenario settings
that force failures. It speaks the mock adapter contract `mocks.adapter/v1`, so a Mocks Studio core can run it and
the conformance suite accepts it.

Run the commands below from the `mocks/openapi/` directory of the repository (the module root), with the
`openapi-mock` command built from `./cmd/openapi-mock` (`go build -o openapi-mock ./cmd/openapi-mock`).

## Serve a document

```
$ openapi-mock serve --spec examples/acme-example-orders/openapi.yaml --name acme-example-orders --control 127.0.0.1:9100
```

- `--spec` is a JSON or YAML OpenAPI 3.0.x or 3.1.x document. Only references inside the document (`#/…`) are followed.
- `--name` is the mock's name (`^[a-z0-9][a-z0-9-]{1,62}$`); it is the `mock.name` of every event.
- `--control` is where the control surface listens. Only the core calls it; it opens and closes scopes.
- `--control-url` (optional) is the URL the manifest advertises for the control surface, when callers reach it by
  another address than `--control`. Default: `http://` plus the address `--control` bound.
- `--safe-field <name>` (repeatable, none by default) names a query or body field whose value may appear in clear in
  an event's payload. A name that looks like a credential (`password`, `pin`, `token`, `secret`, `api_key`…) is refused.
- `--verbose` also logs the name of every header, query and body field of each call, each with the marker `[redacted]`
  in place of its value.

The mock opens **one HTTP listener per scope** (per run). A call belongs to the run whose listener it arrives on.
Open a scope with `POST /_mock/v1/scopes` and `{"run_id":"my-run-1","listener":{"protocol":"http","host":"127.0.0.1","port":18080}}`
(port 0 asks for a free port; the answer names the one bound). Close it with `DELETE /_mock/v1/scopes/my-run-1`.

Write the manifest file without serving (it is the same document `serve` serves at `GET /_mock/v1/manifest`):

```
$ openapi-mock manifest --spec examples/acme-example-orders/openapi.yaml --name acme-example-orders --control-url http://127.0.0.1:9100 --out -
```

`--out <file>` writes the file instead (default `mocks.manifest.json`). Check a running mock with the conformance suite
(this is the acceptance run of the example; it exits 0 when every check passes):

```
$ go -C conformance run ./cmd/mocks-conformance --manifest mocks/openapi/examples/acme-example-orders/mocks.manifest.json
```

## What the mock answers

For a call on a scope's listener, in this order:

1. **Route.** The path is matched against the document's path templates (a literal path beats a templated one; the
   first server's base path, if it has one, is a prefix). No match is `404` `route-unknown`. A matching path whose
   method the document does not declare is `405` `method-not-allowed` with an `Allow` header and the allowed methods.
2. **Validate.** Path, query, header, cookie and body are checked against the operation's parameters and request body.
   A broken request is `400` with a JSON body naming the field's JSON pointer and the rule broken:
   `{"message":…,"reason_code":"request-invalid","pointer":"/body/quantity","rule":"minimum","violations":[…]}`.
   Pointers start with `/path/`, `/query/`, `/header/`, `/cookie/` or `/body`. The `request.received` event's summary
   names the same pointer. The value is never echoed.
3. **Answer.** The operation's lowest 2xx response (else `default`): its `example`, else the first of its `examples`, else
   a value generated from its schema. Generation is deterministic: the same schema gives the same bytes. It uses a
   schema's `const`, `example`, `examples`, `default` or first `enum` member where there is one, a format-correct value
   for `date-time`, `uuid`, `email` and the like, the smallest number inside `minimum`/`maximum`, and cuts recursive schemas.

A fault setting (below) changes the answer *after* routing and takes precedence over a `400`: a mock told to answer 503
answers 503 even to a request that also breaks the document.

## Settings

Set them with `PUT /_mock/v1/scopes/{run_id}/settings` and a JSON object, for example `{"status_code":503}`. The request is
all-or-nothing. `POST /_mock/v1/scopes/{run_id}/reset` returns every setting to its default. Each setting is per run.

| setting | default | range | effect |
|---|---|---|---|
| `status_code` | 0 | 0–599 | every call that reaches an operation is answered with this status; 0 is off; 1–199 act as 0 |
| `operation_status.<id>` | 0 | 0–599 | the same for one operation; wins over `status_code`. `<id>` is the `operationId` in snake_case (`listOrders` → `operation_status.list_orders`); the manifest lists them all |
| `fail_rate` | 0 | 0–1 | the share of calls answered `503` |
| `delay_ms` | 0 | 0–60000 | milliseconds waited before any call is answered; a closing scope ends the wait |

A forced status is answered with the operation's declared response for that status when it declares one, else with a
JSON refusal document (`reason_code` `fault-injected`). A call whose answer a setting changed has `outcome: fault` and
`fault: <setting name>` on its `response.sent` event, and one `fault.injected` event.

## Assert on events

`GET /_mock/v1/events?run_id=my-run-1` is a server-sent event stream; each `data:` line is one `mocks.event/v1` JSON
object. It is live: connect before the call you want to see. Per call you get, in order:

- `request.received` — `summary` is `METHOD template`, plus ` — invalid <pointer> (rule <rule>)` when the request broke the document.
- `fault.injected` — only when a setting changed the answer; `fault` names the setting.
- `response.sent` — `outcome` is `ok`, `fault` or `refused` (400, 404, 405); `latency_ms`; the status is in the summary.

Also `scope.opened`, `scope.closed`, `scope.reset`, `scenario.set` (the settings that were set) and `scope.mismatch` (a call
whose `baggage` names `mocks.run=<another run>`: it is served and recorded under the listener's run).

`correlation_id` is the trace-id of a valid `traceparent` header (`correlation_source: traceparent`), else a generated one
(`generated`); the request and its response share it. A `payload` is `{"redacted":true,"digest":"sha256:…","size":N}`
by default: nothing of the content. With `--safe-field` it is `{"redacted":false,"fields":{…}}` holding only those fields.
No event carries a header, so no `Authorization` value is ever in one.

## Refusals and what to do

At start (`openapi-mock` exits **2**, writes what failed and where to stderr, serves nothing):

| refusal | what to do |
|---|---|
| the document cannot be read, is empty, is not JSON or YAML | fix the path or the syntax named |
| `"openapi"` is not 3.0.x or 3.1.x | convert the document (Swagger 2.0 is not supported) |
| a reference `#/…` does not resolve, or is not local | the message names the reference and where it is; add the component or inline the external file |
| a schema is invalid, an `operationId` is used twice, a parameter is not `in` path, query, header or cookie | fix the location named |
| the document has no operation under `paths` | add one |
| `--name`, `--control-url` or a `--safe-field` is malformed or a credential name | use a name matching the pattern in the message |
| missing `--spec`, `--name` or `--control` | add the flag |

`openapi-mock` exits **1** when the control address cannot be opened (the port is taken): choose another `--control`.

On the control surface every refusal is `{"message":…,"reason_code":…,"allowed":[…]}`; key on `reason_code`:

| status | `reason_code` | what to do |
|---|---|---|
| 422 | `run-id-invalid` | use 3–64 characters of `a-z0-9._-`, starting with a letter or digit |
| 409 | `run-already-open` | close the scope or use another run id; the open scope is untouched |
| 422 | `listener-protocol-unsupported` | ask for `http` (listed in `allowed`) |
| 409 | `listener-unavailable` | ask for another port |
| 422 | `setting-unknown` | use one of the names in `allowed` |
| 422 | `setting-type-invalid` | send a number (an integer for the integer settings) |
| 422 | `setting-out-of-range` | send a value inside the range in the table above |
| 422 | `body-invalid` | send one JSON object |
| 404 | `run-unknown` | open the scope first |

## Limits

- Security schemes are not enforced: no call is refused for a missing credential.
- Object-valued query, header and cookie parameters are not validated; array and scalar ones are. `format` is checked
  for the formats the schema library knows; a `pattern` is checked but never used to generate a value, so a generated
  value for a patterned schema may fail its own pattern.
- Request bodies are validated when JSON or form-encoded; other media types are checked only for presence and `Content-Type`.
  The answer is never negotiated from `Accept`: JSON is preferred, else the first media type by name.
- Request bodies over 1 MiB are refused `400` with rule `maxBytes`.
- A slow event-stream reader that falls 4096 events behind loses events rather than slowing the mock.
