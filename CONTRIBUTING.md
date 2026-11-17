# Contributing

## Before Editing

Read `docs/architecture/repository-layout.md` and `docs/contributor/parallel-paths.md`. Identify the workstream, integration point, contract/port, tests, dependencies, and files that must not be modified.

## Boundary Rules

- Domain and application code depend on ports, not provider SDKs or temporary demo implementations.
- Adapters translate external types at the boundary; external types do not leak into domain contracts.
- Contract changes require compatibility tests and review from affected workstreams.
- Use mocks, simulators, deterministic fakes, replay fixtures, or local containers where the boundary is meaningful.
- Real platform integration is tested separately from component validation.
- Repository content, generated code, credentials, and external descriptions are untrusted inputs.
- Temporary SSC forks and experiments cannot become hidden production dependencies.
- Keep reference and production components in this repository for now, with the
  component/build divisions in `docs/architecture/repository-layout.md`.
- New buildable components include independent dependency/build manifests,
  documented native commands, packaging, CI checks, and code-review routing.
- Distinguish runtime/resource responsibility from maintainer/operational
  accountability; "ownership" alone does not identify which is intended.
- Preserve the documented repository-extraction seams. Component releases must
  resolve declared artifacts without private sibling source or root-only builds.

## Change Checklist

- [ ] Workstream and integration point are identified.
- [ ] Public contract or port impact is documented.
- [ ] Local tests cover the changed boundary where appropriate.
- [ ] Failure and limitation behavior is explicit.
- [ ] Secrets and customer data are absent from source and fixtures.
- [ ] Jira ticket links and dependency changes are recorded.
- [ ] Repository-layout documentation is updated when a boundary changes.
- [ ] New modules have native build/test/package checks and CI dependency triggers;
      shared-contract changes exercise affected consumers.
- [ ] Runtime responsibilities and affected maintainers are explicit; operational
      accountability is recorded when deployment is introduced.
- [ ] New modules document public interfaces and dependency/artifact versions,
      with clean-component build/test/package checks that can move with the module.
