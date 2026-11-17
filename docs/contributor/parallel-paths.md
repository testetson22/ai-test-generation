# Parallel Workstream Guide

Before starting a ticket, identify its workstream and use the corresponding path:

| Jira outcome | Place work in | Add tests in | Do not modify |
|---|---|---|---|
| VI-* | Input/policy adapters and contracts | `tests/contract/`, `tests/component/`, input fixtures | ATG planning, repository commands, VEX publication |
| TF-* | Repository/framework adapters and target metadata | `tests/component/`, `tests/differential/` | VPE policy interpretation, model prompts, provider implementation |
| TG-* | Domain, application, workflow, gates, provenance | `tests/unit/`, `tests/contract/`, `tests/replay/`, `tests/evaluation/` | Vendor SDKs, temporary demo assumptions, direct repository mutation |
| TG-* production runtime | `services/<component>/`, `workers/<profile>/` when implemented | Component-native tests plus shared contract/conformance fixtures | Reference internals, private sibling state, implicit language/platform/harness coupling |
| TG-S1 | `experiments/harness/` and research docs | `tests/replay/` | Production imports and domain contracts |
| TG-S2/TG-S3 | `experiments/providers/` and architecture docs | Contract/replay tests | Unapproved credentials and final ownership assumptions |
| FDI-* | `deploy/final-demo/` and runbooks | `tests/integration/` | ATG domain internals and temporary-harness assumptions |

## Ticket Metadata

Every implementation change should identify:

- Integration point and public contract/port.
- Local test layer and command.
- External dependencies and Jira blockers.
- Files or workstreams not to modify.
- Evidence or replay artifacts produced.
- Runtime/resource responsibilities, separately from maintainer accountability.
- Native build/test/package commands and affected CI checks for a new component.

If a change does not fit one row, stop and update the repository layout or contract before coding.
