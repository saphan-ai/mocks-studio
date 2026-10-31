---
name: dns-mock
description: Test name resolution and DNS filtering without a real resolver. Use when code under test resolves names over UDP, TCP or DNS-over-HTTPS and you need declared records, blocked names, forced SERVFAIL, dropped UDP or slow answers, and a record of every query.
license: Apache-2.0
compatibility: Go 1.26 toolchain; loopback networking. Standard library only.
metadata:
  saphan.origin: mocks-studio
  saphan.mock: dns-mock
  saphan.contract: mocks.adapter/v1
---

# dns-mock

A free DNS and DoH (RFC 1035, RFC 8484) filtering resolver mock. One scope is one loopback port that
answers UDP, TCP and DoH (cleartext HTTP at `/dns-query`, GET and POST) for one run. Names live under
`example.test` or `acme-example.test`; addresses come only from RFC 5737 (`192.0.2.0/24`,
`198.51.100.0/24`, `203.0.113.0/24`) and RFC 3849 (`2001:db8::/32`). Every command below runs from the
`mocks/dns` directory.

## 1. Start it

```
go run ./cmd/dns-mock serve --control 127.0.0.1:9100
```

Leave it running (it prints `control=http://127.0.0.1:9100`). Check it: `go run ./cmd/dns-mock call GET /_mock/v1/health`.

## 2. Open a scope (one run, one port)

```
go run ./cmd/dns-mock call POST /_mock/v1/scopes '{"run_id":"demo-run-1","listener":{"protocol":"dns","host":"127.0.0.1","port":15353}}'
```

Expect `HTTP 201`. `protocol` is `dns` or `http`; both open the same UDP+TCP+DoH listener. A taken port
is `409 listener-unavailable`: pick another.

## 3. Declare records and blocks

```
go run ./cmd/dns-mock set --run demo-run-1 --records '[{"name":"www.example.test","type":"A","value":"192.0.2.10"},{"name":"www.example.test","type":"AAAA","value":"2001:db8::10"},{"name":"alias.example.test","type":"CNAME","value":"www.example.test"},{"name":"example.test","type":"MX","value":"10 mail.example.test"},{"name":"example.test","type":"TXT","value":"v=spf1 -all"}]'
go run ./cmd/dns-mock set --run demo-run-1 --blocked '["ads.example.test"]' --block-answer sinkhole --sinkhole-ip 198.51.100.66
```

- `records`: JSON array of `{name, type, value, ttl?}`; types A, AAAA, CNAME, TXT, MX (MX value is `"<preference> <host>"`). A CNAME owns its name alone.
- `blocked`: names; a blocked name and everything below it is answered by `block_answer` and wins over any record. `block_answer` is `nxdomain` (default), `refused`, or `sinkhole` (the `sinkhole_ip` address; an IPv4 sinkhole answers A and nothing for AAAA, an IPv6 one the reverse).
- An unknown name is `NXDOMAIN`; a known name without the asked type is `NOERROR` with no answer.
- A value outside the zones or the reserved ranges is refused `422 setting-out-of-range` and nothing is applied.

## 4. Ask

```
go run ./cmd/dns-mock ask --server 127.0.0.1:15353 --transport tcp --name www.example.test --type A
go run ./cmd/dns-mock ask --server 127.0.0.1:15353 --transport doh --name ads.example.test --type A
```

Prints `rcode=… answers=N truncated=…` then one line per answer. Exit 0 when an answer arrived, 1 when none did.
`--transport udp` is the third choice. A host that refuses to send UDP to `127.0.0.1` (the Saphan fleet seat
does: EPERM) needs the scope opened with `"host":"::1"` and `--server [::1]:15353`.

## 5. Force a failure

```
go run ./cmd/dns-mock set --run demo-run-1 --servfail=true
go run ./cmd/dns-mock set --run demo-run-1 --servfail=false --drop-udp=true
go run ./cmd/dns-mock set --run demo-run-1 --drop-udp=false --latency-ms 500 --doh-http-status 503
```

| setting | effect |
|---|---|
| `servfail` | every query is answered SERVFAIL |
| `drop_udp` | no answer over UDP: the client retries over TCP or times out; TCP and DoH unaffected |
| `latency_ms` | every answer is held back (0 to 10000) |
| `doh_http_status` | DoH answers that HTTP status (200 to 599) instead of the DNS answer |

Each injected failure emits `fault.injected`. Return to defaults with
`go run ./cmd/dns-mock call POST /_mock/v1/scopes/demo-run-1/reset`.

## 6. Assert a query

Every query is an event: `request.received` (direction `inbound`) and `response.sent`, with payload fields
`qname`, `qtype`, `transport` (`udp`, `tcp` or `doh`) and, on the response, `rcode`. A blocked answer is
`outcome: refused`, `fault: blocked`. A malformed message is a redacted payload and a `FORMERR` answer.

```
go run ./cmd/dns-mock events --run demo-run-1 --for 5s
```

The stream is live (no replay): start `events` first, then ask from another terminal. The same stream is
`GET /_mock/v1/events?run_id=demo-run-1` on the control endpoint.

## 7. Close

```
go run ./cmd/dns-mock call DELETE /_mock/v1/scopes/demo-run-1
```

## What it never does

- It never recurses to a real resolver, signs (DNSSEC), or speaks DoT or zone transfers.
- A credential in a DoH request (an `Authorization` header, a `?token=` query value) is never logged and is never in an event; the log and events name only that one was sent (`credentials=authorization,query-param`).
- It binds loopback only.

## Check the mock itself

```
go -C ../../conformance run ./cmd/mocks-conformance --manifest ../mocks/dns/mocks.manifest.json
```

The suite drives the DoH GET probe only; UDP and TCP are covered by `go test ./...` in this directory.
