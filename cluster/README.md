# SSC Demo Cluster Preparation

`ssc-demo` assumes a functioning OpenShift GitOps installation and does not install the platform operators through its application app-of-apps. This directory contains cluster-side preparation utilities only; it does not modify the `ssc-demo` checkout.

## Current Target

- OpenShift 4.20.x.
- Three or more Ready worker nodes.
- ODF StorageCluster Ready with Ceph health `HEALTH_OK`.
- NooBaa Ready for RHTPA/Quay object storage claims.
- `ocs-storagecluster-cephfs` as the default filesystem StorageClass.
- `ocs-storagecluster-ceph-rbd` available for databases where explicitly requested.
- Required operator packages available from `redhat-operators`.

## Usage

Run after `oc_login_auto` has established the cluster context:

```bash
bash cluster/ssc-demo-cluster.sh --preflight
```

The preflight is read-only. The following actions mutate only cluster resources:

```bash
bash cluster/ssc-demo-cluster.sh --ensure-storage
bash cluster/ssc-demo-cluster.sh --bootstrap-gitops
bash cluster/ssc-demo-cluster.sh --bootstrap-platform-argocd
bash cluster/ssc-demo-cluster.sh --bootstrap-pipelines
```

The bootstrap actions are deliberately separate so operator installation can be verified before the SSC app-of-apps is rendered. They read existing manifests from `ssc-demo`; they do not edit or commit that repository.

Platform ArgoCD bootstrap creates only the `platform-gitops` namespace, RBAC, and ArgoCD instance. It intentionally does not apply the SSC component Applications; that remains the later app-of-apps deployment step.

Pipelines bootstrap creates `openshift-pipelines` before applying the chart and disables the chart's Tekton Chains bootstrap. Chains depends on the SSC RHTAS/Rekor deployment and is intentionally deferred to the app-of-apps.

The GitOps bootstrap clears the stale `startingCSV` in the demo's subscription manifest after applying it. The checked-in manifest requests `openshift-gitops-operator.v1.20.4`, which is not present in the current 4.20 `gitops-1.21` catalog; the catalog currently resolves `v1.21.6`.

## SSC Installation Boundary

After GitOps and Pipelines are healthy, `ssc-demo` remains responsible for rendering its own application stack, credentials, and Tekton resources. The cluster preparation utility does not create those resources or credentials.

The next deployment step must use an uncommitted SSC values override containing the cluster apps domain and generated credentials. Do not commit that file. The SSC runbook's active ODF values must be overridden for this cluster's `stable-4.20` storage setup if the app-of-apps enables ODF; the cluster already has ODF, so the app-of-apps ODF component should remain disabled unless its duplicate-StorageCluster behavior is explicitly confirmed.

## Decisions Recorded

- HPP is not used as the default storage for this target; auto-toolbox provisioned ODF on three local block devices.
- CephFS is the default general-purpose filesystem class; Ceph RBD is available for explicit database claims.
- No changes are made to `ssc-demo` by these utilities.
- The VPE-to-Test-Generation MVP can be developed after the platform baseline is healthy; SCM/PR integration remains conditional on what the demo flow requires.

## edge94 Preparation Status

Applied on Oct 30, 2026 after the auto-toolbox redeployment:

- OpenShift `4.20.12`, three Ready workers, all cluster operators healthy.
- ODF `4.20.21` StorageCluster Ready, Ceph `HEALTH_OK`, NooBaa Ready.
- Three `100Gi` local block PVs bound to the ODF device set.
- `ocs-storagecluster-cephfs` default filesystem class and `ocs-storagecluster-ceph-rbd` available.
- OpenShift GitOps operator `v1.21.6` installed after clearing the stale `v1.20.4` starting-CSV pin from the existing bootstrap manifest at the live subscription.
- OpenShift Pipelines operator `v1.24.2` installed; Tekton CRDs are available.
- `platform-gitops` ArgoCD instance and RBAC installed and Available.
- No SSC component Applications, application credentials, or `ssc-demo` repository files were applied or changed.

The remaining deployment step is the SSC app-of-apps: prepare an uncommitted values override, verify its credentials and domain, then apply it through the `platform-gitops` ArgoCD instance. The cluster-preparation script intentionally stops before that step.
