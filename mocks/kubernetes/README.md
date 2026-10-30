# kubernetes-mock

A Go mock of a slice of the Kubernetes API (namespaces, pods, configmaps, deployments) with watch and scenario
settings. It speaks `mocks.adapter/v1`; see `SKILL.md` for what it covers, the settings and the events.

Run it (from the repository root):

    go -C mocks/kubernetes run ./cmd/kubernetes-mock serve --control 127.0.0.1:9100

A core opens one scope per run (`POST /_mock/v1/scopes`); that scope's listener is a plain-HTTP Kubernetes API
(no TLS, no credentials). Point a client's server URL at it; `/api/v1/namespaces` answers the list of namespaces.

Run the conformance suite against the running mock:

    go -C conformance run ./cmd/mocks-conformance --manifest mocks/kubernetes/mocks.manifest.json

Run its tests: `go -C mocks/kubernetes test -count=1 ./...`
