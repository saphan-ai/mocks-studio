---
name: kubernetes-mock
description: Use when code under test talks to a Kubernetes API server and you need a stand-in you can script - namespaces, pods, configmaps and deployments with list, create, update, delete and WATCH, plus forced errors, throttling, latency and dropped watch streams, observable as mocks.event/v1 events. Not a cluster; read "What it does not cover" first.
license: Apache-2.0
metadata:
  saphan.origin: mocks-studio/mocks/kubernetes
  mock.name: kubernetes-mock
  mock.contract: mocks.adapter/v1
---

# kubernetes-mock

A mock of a slice of the Kubernetes API, written in Go against the public API behaviour. It speaks
`mocks.adapter/v1` (`spec/MOCK_ADAPTER_V1.md`): a control surface for a core, and one plain-HTTP listener per
run, each with its own in-memory cluster. Two runs never share an object.

## What it covers

| group/version | resources | verbs |
|---|---|---|
| `v1` | `namespaces`, `pods`, `configmaps` | get, list, create, update (PUT), delete, watch |
| `apps/v1` | `deployments` | get, list, create, update (PUT), delete, watch |

- Paths as the API has them: `/api/v1/namespaces/{ns}/pods/{name}`, `/api/v1/pods` (all namespaces),
  `/apis/apps/v1/namespaces/{ns}/deployments`. The `default` namespace exists in every run.
- **Discovery**: `/api`, `/apis`, `/apis/apps`, `/api/v1`, `/apis/apps/v1`, `/version`, `/healthz`, `/livez`, `/readyz`.
- **List** takes `labelSelector` (`k=v`, `k==v`, `k!=v`, `k`, `!k`, `k in (a,b)`, `k notin (a,b)`, comma-joined) and
  `fieldSelector` on `metadata.name` / `metadata.namespace` (`=`, `==`, `!=`). Any other field selector is a `400`, never ignored.
  A list answers `{kind: PodList, metadata: {resourceVersion}, items}`.
- **resourceVersion**: one counter per run, grown by every create, update and delete. An update that changes nothing is not a
  change (no new version, no event). A PUT carrying `metadata.resourceVersion` that is not current is `409 Conflict`.
- **Watch** (`?watch=true`): one JSON object per line, `{"type":"ADDED|MODIFIED|DELETED","object":{...}}`. The response
  headers are sent at once, so a watch is registered before the caller's next call.
  - no `resourceVersion` (or `0`): the current objects as `ADDED`, then changes;
  - a `resourceVersion`: every retained change after it, then changes;
  - one that fell out of the retained window (the last 256 changes; `--retain` changes it): `410 Gone` as a `Status`
    with reason `Expired`, the answer a real server gives for "too old resource version";
  - with a `labelSelector`, an object entering the selection arrives as `ADDED`, leaving it as `DELETED`;
  - `timeoutSeconds` ends the stream; a watch whose client cannot keep up (256 queued events) is cut off, so it re-lists.
- **Errors** are `Status` objects (`kind: Status`, `status: Failure`, `code`, `reason`, `details`): `400 BadRequest`,
  `404 NotFound`, `405 MethodNotAllowed`, `409 AlreadyExists`, `409 Conflict`, `410 Expired`, `422 Invalid`
  (with `details.causes[].field`, e.g. `metadata.name`, `spec.containers`, `spec.selector`), and the forced ones below.
- **Validation (422)**: names are RFC 1123 (namespaces: labels); a pod needs `spec.containers` each with `name` and
  `image`; a deployment needs `spec.selector.matchLabels` equal to `spec.template.metadata.labels`, a container, and
  `replicas >= 0`; configmap keys match `[-._a-zA-Z0-9]+`. `metadata.generateName` is honoured.
- Server-set fields: `uid`, `resourceVersion`, `creationTimestamp`, `namespace`; pods start `Pending`; a deployment
  reports every desired replica ready at once and counts `metadata.generation` on a spec change.
- Deleting a namespace deletes what it holds, at once, with a `DELETED` event for each object.

## What it does not cover

Other resources (`secrets`, `services`, `nodes`, ...), subresources (`/status`, `/scale`, `pods/log`), PATCH
(`405`), server-side apply, protobuf (JSON only), table output, `deletecollection`, bookmarks, `limit`/`continue`
paging, `Terminating` namespaces, controllers (a deployment creates no pods), admission, RBAC, authentication
(any caller, any header, is accepted) and TLS (plain HTTP). A real cluster is the only judge of those.

## Run it

```sh
go -C mocks/kubernetes run ./cmd/kubernetes-mock serve --control 127.0.0.1:9100
go -C conformance run ./cmd/mocks-conformance --manifest mocks/kubernetes/mocks.manifest.json
```

`serve --control` prints the control URL on stdout and the request log on stderr. A core opens a scope with
`POST /_mock/v1/scopes {"run_id": "...", "listener": {"protocol": "http", "host": "127.0.0.1", "port": N}}`; the
Kubernetes API of that run is then at `http://127.0.0.1:N`. To point a client at it, use that URL as the server, with
no TLS and no credentials. (A client built with the official Go client library was not run against this mock on the
seat that wrote it; see the repository's return notes. The plain-HTTP tests cover the same discovery, list, create and
watch calls.)

## Settings

All are per run, set with `PUT /_mock/v1/scopes/{run_id}/settings`, returned to defaults by `reset`.

| setting | type, default | effect |
|---|---|---|
| `error_status` | integer, 0 | `0` off; `400`-`599` forces that status, as a `Status`, on every call (`200` is refused as out of range) |
| `throttle` | boolean, false | every call answers `429 TooManyRequests` with `Retry-After: 1` |
| `latency_ms` | integer, 0 (max 60000) | every call waits this long before it is answered |
| `drop_watch_after_events` | integer, 0 | a watch **opened while this is N** ends its stream after N events (replayed ones count); `0` never |

`error_status` wins over `throttle`; `latency_ms` applies first, to every call. The conformance suite can observe
`error_status` and `throttle` through a status; `latency_ms` and `drop_watch_after_events` declare no `effect`
because the suite's probe sees only a status, and they are proved by this module's own tests.

## Events

Read them from `GET /_mock/v1/events?run_id=<id>` (server-sent events, one `mocks.event/v1` object per `data:` line).

| kind | when |
|---|---|
| `request.received` | one per call; `summary` is `METHOD path` |
| `response.sent` | one per call; `outcome` is `ok`, `refused` (status 400+) or `fault` (a setting changed the answer, `fault` names it) |
| `message.delivered` | each change a watch delivers; `summary` names the type (`ADDED`...) |
| `fault.injected` | a setting changed an answer: `fault` is `latency_ms`, `error_status`, `throttle` or `drop_watch_after_events` |
| `scope.opened`, `scope.reset`, `scope.closed`, `scenario.set` | the lifecycle and the settings |
| `scope.mismatch` | a call whose `baggage` says `mocks.run=` another run; it is served and recorded |

All events of one call share a `correlation_id`: the trace-id of a valid `traceparent` header
(`correlation_source: traceparent`), else a generated one. Payloads carry only the safe fields `kind`, `namespace`
and `name` in clear; a call with none (discovery) carries a redacted digest. A watch's `response.sent` is sent when the
stream opens, not when it ends. To assert on a call, filter by `kind`, `fault` and `payload.fields.name`.

## No secret in a log

The mock logs one line per call to stderr: the run, the method, the path, and the **names** of query parameters,
headers and configmap data keys. A name that looks secret (`password`, `token`, `secret`, `credential`, `api key`,
`authorization`, `cookie`) is printed as `name=<redacted>`; no value is ever printed, and none reaches an event.

## Worked example: create a deployment, watch it, drop the watch

The `steps` block is run line by line by `TestSkillWorkedExampleRuns`; `$CONTROL`, `$SCOPE`, `$PORT` are the mock's
addresses and `$RV` is the `resourceVersion` of the last list. `WATCH` opens a stream, `EXPECT` reads from it.

```steps
POST $CONTROL/_mock/v1/scopes {"run_id":"demo-run","listener":{"protocol":"http","host":"127.0.0.1","port":$PORT}}
GET $SCOPE/apis/apps/v1/namespaces/default/deployments
WATCH $SCOPE/apis/apps/v1/namespaces/default/deployments?watch=true&resourceVersion=$RV
POST $SCOPE/apis/apps/v1/namespaces/default/deployments {"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"acme-example-web"},"spec":{"replicas":2,"selector":{"matchLabels":{"app":"acme-example-web"}},"template":{"metadata":{"labels":{"app":"acme-example-web"}},"spec":{"containers":[{"name":"web","image":"registry.invalid/acme-example-web:1"}]}}}}
EXPECT ADDED acme-example-web
PUT $CONTROL/_mock/v1/scopes/demo-run/settings {"drop_watch_after_events":1}
GET $SCOPE/apis/apps/v1/namespaces/default/deployments
WATCH $SCOPE/apis/apps/v1/namespaces/default/deployments?watch=true&resourceVersion=$RV
POST $SCOPE/apis/apps/v1/namespaces/default/deployments {"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"acme-example-api"},"spec":{"replicas":1,"selector":{"matchLabels":{"app":"acme-example-api"}},"template":{"metadata":{"labels":{"app":"acme-example-api"}},"spec":{"containers":[{"name":"api","image":"registry.invalid/acme-example-api:1"}]}}}}
EXPECT ADDED acme-example-api
EXPECT END
EVENT fault.injected drop_watch_after_events
DELETE $CONTROL/_mock/v1/scopes/demo-run
```

Reading it: the first watch sees `ADDED acme-example-web`; with `drop_watch_after_events=1` the second watch
delivers one event and ends, and the run's event stream carries a `fault.injected` event naming the setting. A client
that re-watches after the end must do what it does for any dropped stream: list, then watch from the list's version.
