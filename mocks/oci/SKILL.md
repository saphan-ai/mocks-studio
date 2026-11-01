---
name: oci-registry-mock
description: Use when an agent must test container image push or pull (docker, podman, oras, skopeo, a Go or Python registry client) without a real registry, or must see how a client reacts to a registry that demands a login, rate-limits it or is missing a blob. Runs a free OCI Distribution v2 registry on loopback, one isolated registry per run.
license: Apache-2.0
metadata:
  saphan.origin: mocks-studio
  saphan.mock: oci-mock
  saphan.contract: mocks.adapter/v1
---

# OCI registry mock

A registry for tests. It implements the OCI Distribution Specification v1.1 push and pull API
(`/v2/`, blob uploads monolithic and chunked, cross-repository mount, manifests by tag and digest
for image manifests and indexes, tag lists with pagination, `/v2/_catalog`). Every blob digest is
verified (sha256). Errors use the specification's JSON form. Content is in memory and lives as long as the run.
Not covered: the referrers API, signatures, attestations, garbage collection, token scopes (a token grants every repository).

## Start it

Run from the repository root. The control address is only for the core or for you; the registry
itself listens on the address you give each run.

```
go -C mocks/oci build -o /tmp/oci-mock ./cmd/oci-mock
/tmp/oci-mock serve --control 127.0.0.1:9100
```

It prints `control=http://127.0.0.1:9100`. Open a run (a registry on `127.0.0.1:5000`) and close it:

```
curl -s -X POST http://127.0.0.1:9100/_mock/v1/scopes -H 'Content-Type: application/json' -d '{"run_id":"demo-run","listener":{"protocol":"http","host":"127.0.0.1","port":5000}}'
curl -s -X DELETE http://127.0.0.1:9100/_mock/v1/scopes/demo-run
```

Then point a client at `127.0.0.1:5000` over plain HTTP, for example `skopeo copy --dest-tls-verify=false docker://alpine docker://127.0.0.1:5000/acme-example/app:v1`.
Use fictional repository names such as `acme-example/app`.

## Make it misbehave (settings)

```
curl -s -X PUT http://127.0.0.1:9100/_mock/v1/scopes/demo-run/settings -H 'Content-Type: application/json' -d '{"auth_required":true}'
```

| setting | meaning | what the client sees |
|---|---|---|
| `auth_required` (boolean) | every `/v2/` call needs a bearer token | `401` with `WWW-Authenticate: Bearer realm=".../token"`; fetch a token from `GET /token` with Basic `acme-user` / `acme-example-password` (fictional) |
| `rate_limit` (integer, 0 = off) | requests allowed per rolling 60 s, counting every `/v2/` request | `429 TOOMANYREQUESTS` with `Retry-After` |
| `missing_blob` (string) | comma-separated `sha256:` digests whose blob is gone | `404 BLOB_UNKNOWN` on that blob, stored or not |

`POST /_mock/v1/scopes/demo-run/reset` returns the settings to their defaults and keeps the pushed content.
Each injected failure emits a `fault.injected` event naming the setting.

## Read what happened

`GET /_mock/v1/events?run_id=demo-run` streams `mocks.event/v1` events. Payloads carry only the
safe fields `operation`, `repository`, `reference` and `digest`. The registry password, the token
and blob bytes are never logged and never in an event.

## Check the mock itself

```
go -C conformance run ./cmd/mocks-conformance --control http://127.0.0.1:9100
```

With the mock serving on `127.0.0.1:9100`, `fail=0` means the adapter contract holds.
