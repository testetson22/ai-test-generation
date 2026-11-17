# ATG Control Service

Initial production control component for FLPATH-5048. This module is intentionally
standalone from the Python reference and builds with Go 1.26.2 or newer.

```bash
make check
make race
```

`make package` creates a local binary archive. OCI image composition is deferred
until the deployment platform and target architecture are selected; the module
does not depend on the Docker daemon to build or test.

The current bootstrap exposes liveness and readiness only. Readiness remains
blocked until durable state, artifact storage, and an execution provider are
implemented and initialized. Supplying configuration strings does not make an
unimplemented backend ready. No simulated backend is included in the production
binary.

Run locally with `go run ./cmd/atg-control`. `GET /health/live` reports process
liveness; `GET /health/ready` reports runtime dependency initialization and returns
503 while any required dependency is unavailable. The service does not execute
repository commands in its own process.

The `internal/provider` package defines the normalized provider boundary, and
`internal/store/postgres` contains the durable ledger/migration implementation.
The controller and repository are not yet wired into the executable's HTTP
composition; PostgreSQL integration tests run when `ATG_TEST_DATABASE_URL` is set.
`internal/testkit` contains only deterministic development/test substitutes; the
production executable does not import or select them. S3-compatible storage,
HTTP task operations and a real provider adapter are added in later steps.
See `docs/architecture/flpath-5048-runtime.md` for the selected boundary.

`internal/materialization` reconstructs the approved generated-test files only
after checking the patch, materialized-file, base-file, subject, and path
identities; it rejects symlinks and non-regular targets, and verifies exact
preimages before atomic writes. `internal/artifact.FileStore` is a durable local
content-addressed development adapter, not the production S3-compatible store.
Materialization requires an independent workspace-attestor port; the test echo
adapter is not production source attestation. `internal/report` accepts only the
profile-selected `application/xml` report format and bounded JUnit XML, preserving
failed, skipped, missing, and unexpected test identities without asserting pass.
`execution.DecodeBoundProfile` strictly decodes profile bytes against the task's
artifact and approval digests but does not authenticate the reviewer or execute
profile commands; those require the approved authorization and worker adapters.
`internal/evidence.Publisher` verifies stored evidence bytes, records a provisional
result before cleanup, then publishes and records a final result only after cleanup
and termination are known. The PostgreSQL ledger binds both result deliveries to
the expected task revisions. `execution-result.schema.json` describes operational
evidence and does not claim test pass; no application-specific commands or worker
platform are selected by these framework-neutral components.
