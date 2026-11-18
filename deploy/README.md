# Deployment Composition

Deployment files compose the ATG service with external environments. They do not define ATG domain contracts.

- `local/`: temporary local development composition.
- `development-harness/`: development-hosted test-generation integration guidance.
- `final-demo/`: separately approved final-demo composition for A&P/VPE and ATG.

Use GitOps for shared environments. Keep credentials in approved secret references, not source. Record environment-specific workarounds separately from portable service configuration.
