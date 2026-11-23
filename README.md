# AI Test Generation

AI Test Generation (ATG) provides a driver-agnostic, evidence-driven service for the `Analyze -> Plan -> Code -> Execute` lifecycle.

## Repository Status

This repository is the product-oriented repository for current implementation experiments. Product ownership, final project/repository conventions, and long-term release ownership remain subject to confirmation.

Keep contracts, the Python reference, and new production components in this
repository for now. A shared repository does not require a shared language,
process, image, or build. Production components must have explicit interfaces
and independently runnable builds/tests; see the
[component and build boundaries](docs/architecture/repository-layout.md).

The legacy demo and any forks are temporary development and compatibility
material. They do not define the ATG architecture or authoritative Java target.

## Start Here

- `CONTRIBUTING.md` - contributor workflow and boundary rules.
- `docs/architecture/repository-layout.md` - workstream-to-path integration map.
- `docs/architecture/flpath-5048-runtime.md` - initial production controller runtime and storage boundary.
- `docs/architecture/run-state.md` - lifecycle invariants and required controller task/attempt recovery model.
- `docs/contributor/parallel-paths.md` - where each parallel workstream places changes.
- `contracts/README.md` - versioned contract and compatibility rules.
- `tests/README.md` - local, component, replay, and integration validation.
- `experiments/README.md` - temporary harness and provider experiments.
- `deploy/README.md` - development-harness and final-demo composition boundaries.
- `deploy/development-harness/README.md` - standalone local execution, validation, and legacy demo reuse boundaries.
- `services/generation-worker/` - bounded generation-stage module; SDK-free checks are available, live integration remains gated and unverified.

## Current Target

The ATG controller's initial production runtime is Go, independent of the Python
reference and target application. Profile-driven execution starts with the
Java 17/Spring Boot sample application. The VPE/A&P-selected Java 21/Quarkus fixture is
the second-profile portability milestone; it does not block first-target service
development. See the
[initial runtime boundary](docs/architecture/flpath-5048-runtime.md).
