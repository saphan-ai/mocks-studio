# acme-example-orders

A fictional orders API (`openapi.yaml`, OpenAPI 3.1, three operations) served as a mock by `openapi-mock`.
`listOrders` answers from the document's `examples`; `getOrder` has none, so its answer is generated from the schema; `createOrder` validates its request body.
`mocks.manifest.json` is what `openapi-mock manifest --spec openapi.yaml --name acme-example-orders --control-url http://127.0.0.1:9100 --out mocks.manifest.json` writes, and `serve` serves.
Serve it from `mocks/openapi/` with `openapi-mock serve --spec examples/acme-example-orders/openapi.yaml --name acme-example-orders --control 127.0.0.1:9100`.
Check it from the repository root with `go -C conformance run ./cmd/mocks-conformance --manifest mocks/openapi/examples/acme-example-orders/mocks.manifest.json`; it exits 0.
