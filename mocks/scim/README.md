# scim-mock

A free SCIM 2.0 service-provider mock (RFC 7643, RFC 7644) for testing user provisioning without a real
identity system. It speaks `mocks.adapter/v1`; standard library only; Apache-2.0.

- Users and Groups: create, get, list with `filter` and paging, `PUT`, `PATCH`, `DELETE`, ETag and `If-Match`.
- Scenario settings per run: `error_status`, `latency_ms`, `rate_limit`. Secrets never reach a log or an event.

From this directory: `go run ./cmd/scim-mock serve --control 127.0.0.1:9100` and `go test -count=1 ./...`.

How to drive it: [SKILL.md](SKILL.md). Conformance, from the repository root:
`go -C conformance run ./cmd/mocks-conformance --manifest mocks/scim/mocks.manifest.json`.
