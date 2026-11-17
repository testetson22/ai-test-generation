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
