# dns-mock

A free DNS and DNS-over-HTTPS filtering resolver mock (`mocks.adapter/v1`), standard library only.

One scope is one loopback port answering UDP, TCP and DoH (RFC 1035, RFC 8484) from the run's declared
records (A, AAAA, CNAME, TXT, MX) and blocked names; every query is an event.

- Start: `go run ./cmd/dns-mock serve --control 127.0.0.1:9100` (from this directory).
- Use it: see [SKILL.md](SKILL.md) — declare records and blocks, force SERVFAIL, dropped UDP or latency, assert a query.
- Test it: `go test -count=1 ./...`; the suite: `go -C ../../conformance run ./cmd/mocks-conformance --manifest ../mocks/dns/mocks.manifest.json`.
- Names only under `example.test` and `acme-example.test`; addresses only from RFC 5737 and RFC 3849 ranges.
- No secret in a log or an event: DoH credentials are named, never printed.
- License: Apache-2.0.
