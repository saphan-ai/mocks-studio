---
name: s3-mock
description: Test code that uses S3-compatible object storage against a free local mock instead of a real store. Use when a test needs buckets, objects, multipart upload, presigned URLs or SigV4-signed requests, or needs slow reads, SlowDown throttling, forced errors or missing keys on demand. Not for AWS S3 with IAM, bucket policies, versioning or events (a paid pack).
license: Apache-2.0
metadata:
  saphan.origin: mocks-studio
  saphan.contract: mocks.adapter/v1
  saphan.mock: s3-mock
---

# S3 mock

A mock of the S3 REST API as open-source compatible stores speak it, written for the
`mocks.adapter/v1` contract (`spec/MOCK_ADAPTER_V1.md`). Standard library only. The core opens one
scope per run: one HTTP listener with its own buckets, its own settings and its own events.

## Start it

From `mocks/s3`:

```
go run ./cmd/s3-mock serve --control 127.0.0.1:9100
```

The control endpoint (`/_mock/v1/...`, spec §4) is for the core, not for tests. A core opens a scope
(`POST /_mock/v1/scopes`) and hands your code the listener's host and port. Check the mock yourself
with the conformance suite, from the repository root, while it runs:

```
go -C conformance run ./cmd/mocks-conformance --manifest mocks/s3/mocks.manifest.json
```

## Point your code at it

- Endpoint: `http://<listener host>:<port>`, **path-style** addressing (`/<bucket>/<key>`); no virtual-host style.
- Region: any (`us-east-1`); service `s3`. Sign with SigV4, header or query (presigned).
- Credentials: the scope's settings `access_key_id` and `secret_access_key`. The defaults are the
  fictional `AKIAEXAMPLEKEY000001` and a fictional secret; set both per run, never use a real key.
- Use fictional bucket names such as `acme-example-data`.
- Send `x-amz-content-sha256` (the body's hex SHA-256, or `UNSIGNED-PAYLOAD`). `aws-chunked` streaming
  bodies are refused `501 NotImplemented`. Signed requests must be within 15 minutes of the mock's clock.

## What it serves

| operation | notes |
|---|---|
| ListBuckets, CreateBucket, HeadBucket, DeleteBucket | `BucketNotEmpty` while objects remain |
| PutObject, GetObject, HeadObject, DeleteObject | `ETag` is the body's MD5; `Content-MD5` checked (`BadDigest`, `InvalidDigest`); `Range` with `206` and `416 InvalidRange` |
| ListObjects (v1) and ListObjectsV2 | `prefix`, `delimiter`, `max-keys`, `start-after`, `continuation-token` |
| CreateMultipartUpload, UploadPart, CompleteMultipartUpload, AbortMultipartUpload | ETag `<md5 of part md5s>-<n>`; every part but the last must reach `min_part_bytes` (`EntityTooSmall`) |
| presigned GET and PUT | `X-Amz-*` query signature, `X-Amz-Expires` enforced |

Errors are S3's XML (`<Error><Code>…</Code><Message>…</Message>…`): `NoSuchKey`, `NoSuchBucket`,
`NoSuchUpload`, `InvalidPart`, `SignatureDoesNotMatch`, `InvalidAccessKeyId`, `AccessDenied`,
`RequestTimeTooSkewed`, `SlowDown`, and more. Not served: CopyObject, DeleteObjects, versioning,
policies, IAM, events, virtual-host addressing (`501 NotImplemented`).

## Scenarios (settings)

Set them on a run (`PUT /_mock/v1/scopes/<run_id>/settings`); `reset` returns them to the defaults.

| setting | default | effect |
|---|---|---|
| `get_latency_ms` | 0 | every GetObject waits this long (slow read) |
| `slow_down` | false | every request answers `503 SlowDown` |
| `error_status` | 0 | every request answers this status (0, 400, 403, 404, 409, 500, 501, 503) |
| `missing_key_prefix` | "" | GET and HEAD of keys with this prefix answer `NoSuchKey` |
| `min_part_bytes` | 5242880 | smallest non-final multipart part |
| `access_key_id`, `secret_access_key` | fictional | the credentials the scope verifies |

Faults apply before authentication, so they show on any call. Each emits `fault.injected`, and the
`response.sent` event carries `outcome: fault` and `fault: <setting>`.

## What it never exposes

Events carry only `bucket`, `key` and `operation` in clear. Object bodies, the secret access key, the
`Authorization` header and presigned signatures appear in no event and in no log line: the access log
lists header and query-parameter names, never values.

## Verify the mock itself

From `mocks/s3`:

```
go test -count=1 ./...
```
