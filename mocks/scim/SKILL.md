---
name: scim-mock
description: Test user provisioning against a free SCIM 2.0 service-provider mock (RFC 7643 and RFC 7644) instead of a real identity system. Use when code, a connector or an agent must create, read, filter, patch or delete Users and Groups over SCIM, check ETag and If-Match handling, or see how a client copes with a 429, a slow answer or a forced 5xx.
license: Apache-2.0
compatibility: Go 1.26 or newer to build and run (standard library only); curl for the examples. Loopback networking must be permitted on the host.
metadata:
  saphan.origin: mocks-studio
  saphan.mock: scim-mock
  saphan.contract: mocks.adapter/v1
---

# SCIM 2.0 mock

A SCIM service provider that keeps its data in memory, one isolated set of Users and Groups per run. It
accepts any bearer token and never records it. All names below are fictional; use `@acme-example.test`
addresses.

## Use when

- A provisioning client, SCIM connector or test needs a server that answers like an identity provider.
- You must prove a client sends the right `filter`, `PATCH` operations or `If-Match` header.
- You want a scripted failure: a forced status, a delay, or a rate limit.

## Do not use when

- You need `/Bulk`, `/Me` or enterprise-extension attributes: they are not implemented.
- You need real authentication: any token is accepted.

## Start it

From `mocks/scim` in the mocks-studio repository (loopback addresses only):

```
go run ./cmd/scim-mock serve --control 127.0.0.1:9100
```

Open one run on a port you choose, from any directory. The run is the listener: every call to that port
belongs to that run.

```
curl -s -X POST http://127.0.0.1:9100/_mock/v1/scopes -H 'Content-Type: application/json' -d '{"run_id":"acme-example-run-1","listener":{"protocol":"http","host":"127.0.0.1","port":18080}}'
```

Close it when done: `curl -s -X DELETE http://127.0.0.1:9100/_mock/v1/scopes/acme-example-run-1`.

## Call it

The SCIM base is `http://127.0.0.1:18080` (a `/v2` or `/scim/v2` prefix is also accepted).

```
curl -s -X POST http://127.0.0.1:18080/Users -H 'Content-Type: application/scim+json' -d '{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"userName":"alice@acme-example.test","displayName":"Alice Example","password":"example-only"}'
curl -s -G http://127.0.0.1:18080/Users --data-urlencode 'filter=userName sw "alice" and active eq true' --data-urlencode 'count=10'
```

Supported: `POST/GET/PUT/PATCH/DELETE` on `/Users` and `/Groups`; `filter` with `eq ne co sw ew gt ge lt le pr`, `and`,
`or`, `not`, parentheses and `attr[...]`; `startIndex` and `count`; `PATCH` `add`, `replace`, `remove` with or without a
path, including `emails[type eq "work"].value`; `/ServiceProviderConfig`, `/Schemas`, `/ResourceTypes`.

- Each resource has `meta.version` and an `ETag` header (`W/"1"`, `W/"2"`, ...). A `PUT`, `PATCH` or `DELETE` whose
  `If-Match` is not the current ETag is refused with `412`; `If-Match: *` always matches. A change that alters
  nothing keeps the version.
- Errors are RFC 7644 §3.12 documents: `status` as a string, `scimType` (`uniqueness` 409, `invalidFilter`,
  `invalidValue`, `invalidSyntax`, `invalidPath`, `noTarget`, `mutability` 400), and a `detail` that says what to do next.
- `userName` is unique per run, case-insensitively. A group member must be an existing user or group.
- `password` is accepted and discarded; it is never stored, returned or logged.

## Script a failure

Settings are set per run through the control surface; the next SCIM call obeys them.

```
curl -s -X PUT http://127.0.0.1:9100/_mock/v1/scopes/acme-example-run-1/settings -H 'Content-Type: application/json' -d '{"error_status":503}'
```

| setting | meaning | off |
|---|---|---|
| `error_status` | every SCIM call answers this HTTP status (100 to 599) | `0` |
| `latency_ms` | every call waits this long first (up to 10000) | `0` |
| `rate_limit` | calls allowed per 60-second window; the next gets `429` with `Retry-After` | `0` |

Reset the run to defaults with `curl -s -X POST http://127.0.0.1:9100/_mock/v1/scopes/acme-example-run-1/reset`.
Each injected failure emits a `fault.injected` event.

## See what happened

```
curl -s -N 'http://127.0.0.1:9100/_mock/v1/events?run_id=acme-example-run-1'
```

One `mocks.event/v1` JSON object per `data:` line: `request.received`, `response.sent`, `fault.injected`,
`scenario.set`, `scope.*`. Payloads carry only the declared safe fields: `method`, `path`, `resource_type`,
`user_name`, `display_name`, `active`. Nothing else of a request body, and no header, is exported or logged;
the log says only whether an `Authorization` header was present.

## Prove the mock still conforms

From the repository root:

```
go -C conformance run ./cmd/mocks-conformance --manifest ../mocks/scim/mocks.manifest.json
```

It must end with `fail=0`. Run it while the mock serves on `127.0.0.1:9100`.

## Limits

- Settings cannot be set from SCIM calls; only the control surface changes them.
- `rate_limit` has no conformance `effect` (the suite cannot observe a limit from one probe), so it is checked by this
  mock's own tests only.
- Data lives in memory and ends with the run or the process.
