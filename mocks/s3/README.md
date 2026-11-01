# s3-mock

A free S3-compatible object-store mock for `mocks.adapter/v1`. Standard library only, Apache-2.0.

- Buckets, objects (`Range`, `ETag`, `Content-MD5`), list v2, multipart upload, presigned URLs, SigV4.
- Scenarios as settings: slow reads, `SlowDown`, forced errors, missing keys.
- Path-style addressing, one listener per run, fictional credentials by default.

Run, from this directory: `go run ./cmd/s3-mock serve --control 127.0.0.1:9100`

Test: `go test -count=1 ./...` (includes the conformance suite).

How to use it from a test: see `SKILL.md`. Not covered: IAM, policies, versioning, events (a paid pack).
