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

The default executable exposes liveness and fail-closed production execution
routes. Default-mode readiness remains blocked until its production dependencies
are initialized; configuration strings alone do not make a backend ready.
Explicit loopback-only development modes are separate: `offline-simulation`
performs no external work, while `local-development` uses real PostgreSQL,
development FileStore artifacts, and isolated local OCI workers. Local readiness
does not claim production readiness or live generation.

Run locally with `go run ./cmd/atg-control`. `GET /health/live` reports process
liveness; `GET /health/ready` reports runtime dependency initialization and returns
503 while any required dependency is unavailable. The service does not execute
repository commands in its own process.

The `internal/provider` package defines the normalized provider boundary, and
`internal/store/postgres` contains the durable ledger/migration implementation.
The HTTP component defines `POST /execution-pairs`, `GET /execution-pairs/{id}`,
`GET /executions/{id}`, and revision-checked `POST /execution-pairs/{id}/cancel`.
Creation accepts run/generation/profile/selection references only; an injected
resolver must produce matching approved task specifications. Authentication,
authorization, resolution, and audit are required ports, and unconfigured
operations return 503. The executable deliberately wires none of these adapters
until their production implementations are selected. PostgreSQL integration tests
run when `ATG_TEST_DATABASE_URL` is set. `internal/testkit` contains only
deterministic development/test substitutes; the production executable does not
import or select them. S3-compatible storage and a concrete cluster API client
remain open. `internal/provider/tekton` supplies provisional resource rendering,
identity checks, and observation normalization against pinned fixtures; no
production provider or runtime wiring is selected.
See [`flpath-5048-runtime.md`](../../docs/architecture/flpath-5048-runtime.md)
for the selected control boundary and
[`repository-layout.md`](../../docs/architecture/repository-layout.md#provisional-phase-runtime-boundaries)
for phase-language and harness guidance.

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
and termination are known. The PostgreSQL ledger binds both deliveries to their
attempt and expected task revision, retaining earlier attempts' evidence across
retries without overwriting the current task's result. Migration
`0002_attempt_results.sql` backfills existing deliveries and recorded termination
proof; the initial migration remains unchanged. `execution-result.schema.json`
describes operational
evidence and does not claim test pass; no application-specific commands or worker
platform are selected by these framework-neutral components.

## Real Local Execution

The validated default `local-development` mode runs an **execution-only,
artifact-input, patch-only** flow. It does not create approvals, model results,
pushes, or PRs. The Java 17 worker/profile live in `workers/java17-maven/`.
The additional staged-generation code below is opt-in, default-closed, and
offline-validated only; it does not change that default.

Required settings are `ATG_DATABASE_URL`, `ATG_ARTIFACT_ROOT`,
`ATG_LOCAL_PROFILE`, and `ATG_DEVELOPMENT_BEARER_TOKEN` (at least 24 characters).
`ATG_LOCAL_ENGINE` defaults to `podman`; `ATG_LOCAL_IMAGE` defaults to
`localhost/atg-java17-maven-v3:local`. `ATG_LISTEN_ADDR` must contain a literal
loopback IP. The current local adapter requires a rootless Linux Podman runtime
with cgroup v2; macOS requires its Linux VM. Docker can build/run the same image
recipe, but is not yet a supported control-service engine adapter.

The service resolves the image once to an immutable **local image ID**, stores a
profile/image binding artifact, and uses that artifact's digest for admission
and dispatch. A tag change does not silently rebind a running service; restarting
with a different image produces a different profile binding. This is not a
published registry digest or production profile approval.
Existing runs remain readable, but cancellation against an inactive profile
returns `run_profile_not_active`; restart with the original profile/image binding
to reconcile or cancel its in-flight work.

The authenticated development endpoints are:

- `POST /development/test-artifacts`: validate and retain a bounded JSON artifact
  containing new Java test files and independently supplied expected test IDs.
- `POST /development/test-generation-runs`: admit a run with
  `input_artifact_digest` and an `Idempotency-Key`; source/profile are server-owned.
- `GET /development/test-generation-runs/{runID}`: persisted status and result
  references, including explicit execution-only mode.
- `GET /development/test-generation-runs/{runID}/operations`: durable nonsecret
  operation identities, states, and retained-result references.
- `POST /development/test-generation-runs/{runID}/cancel`: revision-checked
  cancellation with `expected_revision` and `Idempotency-Key`.
- `POST /development/test-generation-runs/{runID}/reconcile`: revision-checked
  recovery of a verified, completed Plan receipt after an unknown completion
  acknowledgement; this endpoint never starts or resubmits a model worker.
- `GET /development/artifacts/{digest}`: bounded, digest-verified artifact bytes.

`internal/localservice` drives configured patch-only runs using fenced PostgreSQL
claims and a write-ahead operation journal. Worker names derive from durable
operation keys. Restart reconciles existing workers; missing workers after an
ambiguous submission produce `outcome_unknown`, not an automatic replacement.
The explicit Plan-recovery path is narrower: it requires a stopped worker with a
valid completed receipt bound to the original operation input before reopening
the human review checkpoint. A supplied artifact is never recorded as a model
attempt.

Each source/Maven operation has a fresh bounded tmpfs checkout/cache, nonroot
UID, read-only root, dropped capabilities, CPU/memory/PID/file-size/deadline
limits, and no model/SCM/database credentials or host/socket mounts. The explicit
development network policy permits public egress; it is not an enforced
allowlist. Maven resolves only through public Central with controlled settings.
This is a development sandbox, not proof of adversarial-code containment.

The controller retains a provisional result manifest before cleanup and a final
manifest only after confirmed cleanup. Reports must contain the selected test
IDs with positive counts and no missing, unexpected, failed, or skipped tests;
exit zero alone is insufficient. Cancellation retains partial evidence and
confirms termination before the cancelled event. State and artifacts must remain
available across controller restarts; do not delete their persistent directories
or run database-resetting tests against the service's database.

See the [local run instructions](../../deploy/development-harness/README.md#standalone-local-execution)
for build/start/validation commands. `scripts/validate_local_execution.py` is a
key-free-with-respect-to-models, opt-in HTTP acceptance client, not a service
dependency. Native unit tests remain model-free; PostgreSQL and OCI integration
tests require their explicit test configuration.

## Staged Generation Gate

`ATG_LOCAL_INPUT_MODE=model_generation` selects controller-owned Analyze/Plan/Code
handoffs. `ATG_ALLOW_MODEL_CALLS=true` is also required. The permission check
precedes dependency probes, artifact creation, credential access, or database
startup; merely selecting the mode or installing an SDK does not authorize calls.

`services/generation-worker/` is an independent bounded Python worker, not a
lifecycle controller or a dependency on the Python reference services. Its
versioned task/result schemas are
`contracts/v1/generation-stage-{task,result}.schema.json`. One shared
`workers/generation/Containerfile` builds harness-specific package extras for
local or eventual Tekton use. It requires an explicitly selected digest-pinned
Python base. The root `.dockerignore` sends only its explicit build inputs,
excluding local state, credentials, Git history and virtualenvs.

After separately permitted setup, model mode requires:

- `ATG_GENERATION_POLICY`: nonsecret policy selecting the HTTPS OpenAI-compatible
  endpoint/model, harness, context files and bounded request/token/output limits.
- `ATG_GENERATION_IMAGE`: an existing local image, frozen into the policy binding.
  Readiness does not pull images or call the model.
- `ATG_GENERATION_CREDENTIAL_FILE`: a private bounded regular nonsymlink key file.
  The key is admitted separately into worker tmpfs, never task JSON, command
  arguments, container metadata, prompts, artifacts, or the Maven environment.
- `ATG_DEVELOPMENT_REVIEWER_ID`, `ATG_DEVELOPMENT_REVIEWER_ROLE` (`developer` or
  `reviewer`), and `ATG_DEVELOPMENT_REVIEW_AUTHORIZATION_REF`: actual configured
  attribution; no actor or role is accepted from model output or the review body.
- Optional `ATG_GENERATION_MAX_ATTEMPTS` (default 2, maximum 6) and
  `ATG_GENERATION_REVIEW_TTL` (default `1h`, range `1m` through `24h`).

Create accepts `objective`, not caller-selected context, models, or commands.
Each phase has a durable identity and retained inputs/outputs. The run pauses as
`waiting_review` after Plan and Code. Completed phases are reused after review
or restart; review metadata cannot mutate their input hashes or become model
context. No later phase/test is dispatched before its checkpoint approval.

`POST /development/test-generation-runs/{runID}/reviews` accepts
`expected_revision`, `decision_id`, `checkpoint_id`, `stage`, `artifact_ref`,
`action` (`approve`/`reject`), and nonempty `rationale`. Code approval additionally
requires independently supplied `expected_test_ids`. The handler retains the
checked test artifact from the reviewed result; model-suggested IDs never become
execution authority automatically. Decisions are payload-bound and replayable;
expired/stale/cross-policy/wrong-artifact decisions conflict. Expired checkpoints
can be cancelled, not silently approved.

Rejection and explicit model `blocked`/`inconclusive` outcomes stop later work.
Repair attempts have fresh phase keys/reviews and bounded prior-candidate and
verified failure-log context. Go alone authorizes retries. Cancellation and final
cleanup reconcile both worker roles; a missing ambiguous job cannot trigger a
replacement model call. Historical failed observations remain retained.

Pydantic AI 2.68.0 is installed from the Python 3.12 hash lock in the local
generation image. SDK imports, a network-disabled mock request, `pip check`, and
the exact rootless resource-limited worker probe pass without enabling real model
calls. Migration
`0005_generation_reviews.sql` is applied to the fresh disposable development
database, and focused PostgreSQL migration/restart tests pass. The running
service remains in artifact-input mode with `live_generation=false`.

No EnMaaS request or source checkout has been made from this generation path. The
credential-file setting, task-specific policy/context, provider request behavior,
reasoning parameter mapping, and usage/spend budget still need explicit setup.
**Live Aider remains blocked** because its hard request/token and shell/tool
controls are unverified. Its package branch does not bypass the worker's live
block, and readiness rejects unsupported/co-installed harness configurations.
