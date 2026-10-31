# oci-mock

A free OCI Distribution v2 registry mock for Mocks Studio (`mocks.adapter/v1`), Apache-2.0.
Push, pull, manifests, tags and blob uploads, with auth, a rate limit and missing blobs as scenario settings.

    go -C mocks/oci build -o /tmp/oci-mock ./cmd/oci-mock
    /tmp/oci-mock serve --control 127.0.0.1:9100

- Standard library only; one isolated registry per run; loopback by design.
- `SKILL.md` tells an agent how to open a run, set scenarios and read events.
- Tests: `go -C mocks/oci test -count=1 -race ./...`
- Contract check: `go -C conformance run ./cmd/mocks-conformance --control http://127.0.0.1:9100`
- Fictional credentials (`acme-user`, `acme-example-password`); none is ever logged or put in an event.
