#!/usr/bin/env bash
set -euo pipefail

# Cluster-side preparation for the SSC demo. This script never edits the
# ssc-demo checkout; it only reads its existing manifests and applies the
# explicitly requested cluster bootstrap resources.

SSC_DEMO_DIR="${SSC_DEMO_DIR:-/Users/tstetson/Public/workspaces/ssc-demo}"
ACTION="preflight"

usage() {
  cat <<'EOF'
Usage: ssc-demo-cluster.sh [--preflight|--ensure-storage|--bootstrap-gitops|--bootstrap-platform-argocd|--bootstrap-pipelines]

Actions:
  --preflight             Read-only cluster and SSC prerequisite checks (default)
  --ensure-storage        Ensure ODF CephFS is the default filesystem StorageClass
  --bootstrap-gitops      Apply the existing SSC GitOps operator bootstrap manifests
  --bootstrap-platform-argocd  Apply the SSC platform ArgoCD instance and RBAC
  --bootstrap-pipelines   Render/apply the existing SSC Pipelines operator chart

Environment:
  SSC_DEMO_DIR             Path to an untouched ssc-demo checkout
EOF
}

while (($#)); do
  case "$1" in
    --preflight|--ensure-storage|--bootstrap-gitops|--bootstrap-platform-argocd|--bootstrap-pipelines)
      ACTION="$1"
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "Unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
  shift
done

require_file() {
  [[ -f "$1" ]] || { echo "Missing required file: $1" >&2; exit 1; }
}

check_login() {
  oc whoami >/dev/null
  echo "User: $(oc whoami)"
  echo "Server: $(oc whoami --show-server)"
}

check_cluster() {
  local server_version
  server_version="$(oc version | awk '/Server Version:/ {print $3; exit}')"
  [[ "$server_version" == 4.20.* ]] || {
    echo "Expected OpenShift 4.20.x; found ${server_version:-unknown}" >&2
    exit 1
  }
  local ready_workers
  ready_workers="$(oc get nodes -l node-role.kubernetes.io/worker --no-headers | awk '$2 == "Ready" {count++} END {print count+0}')"
  (( ready_workers >= 3 )) || { echo "Expected at least three Ready workers; found $ready_workers" >&2; exit 1; }
  oc wait --for=condition=Available --timeout=5m clusteroperator --all >/dev/null
  oc wait --for=condition=Degraded=false --timeout=5m clusteroperator --all >/dev/null
  echo "OpenShift: $server_version; Ready workers: $ready_workers"
}

check_storage() {
  local phase health noobaa_phase default_fs
  phase="$(oc get storagecluster ocs-storagecluster -n openshift-storage -o jsonpath='{.status.phase}')"
  health="$(oc get cephcluster -n openshift-storage -o jsonpath='{.items[0].status.ceph.health}')"
  noobaa_phase="$(oc get noobaa noobaa -n openshift-storage -o jsonpath='{.status.phase}')"
  default_fs="$(oc get storageclass ocs-storagecluster-cephfs -o jsonpath='{.metadata.annotations.storageclass\.kubernetes\.io/is-default-class}')"
  [[ "$phase" == Ready && "$health" == HEALTH_OK && "$noobaa_phase" == Ready ]] || {
    echo "ODF is not ready: StorageCluster=$phase Ceph=$health NooBaa=$noobaa_phase" >&2
    exit 1
  }
  [[ "$default_fs" == true ]] || echo "WARN: CephFS is not the default StorageClass; run --ensure-storage"
  oc get pv -o custom-columns='NAME:.metadata.name,CAPACITY:.spec.capacity.storage,CLASS:.spec.storageClassName,STATUS:.status.phase' | sed -n '1,12p'
  echo "ODF StorageCluster=Ready Ceph=HEALTH_OK NooBaa=Ready"
}

check_packages() {
  local package
  for package in openshift-gitops-operator openshift-pipelines-operator-rh rhtpa-operator rhtas-operator quay-operator rhbk-operator; do
    oc get packagemanifest "$package" >/dev/null || {
      echo "Missing operator package: $package" >&2
      exit 1
    }
  done
  echo "Required operator packages are available in the catalog"
}

preflight() {
  check_login
  check_cluster
  check_storage
  check_packages
  echo "Preflight passed. No cluster resources were changed."
}

ensure_storage() {
  check_login
  check_storage
  oc patch storageclass ocs-storagecluster-cephfs --type=merge \
    -p '{"metadata":{"annotations":{"storageclass.kubernetes.io/is-default-class":"true"}}}' >/dev/null
  echo "Ensured ocs-storagecluster-cephfs is the default filesystem StorageClass"
}

bootstrap_gitops() {
  require_file "$SSC_DEMO_DIR/bootstrap/platform-gitops/gitops-operator/namespace-openshift-gitops-operator.yaml"
  require_file "$SSC_DEMO_DIR/bootstrap/platform-gitops/gitops-operator/olm-gitops-operatorgroup.yaml"
  require_file "$SSC_DEMO_DIR/bootstrap/platform-gitops/gitops-operator/olm-gitops-subscription.yaml"
  check_login
  oc apply \
    -f "$SSC_DEMO_DIR/bootstrap/platform-gitops/gitops-operator/namespace-openshift-gitops-operator.yaml" \
    -f "$SSC_DEMO_DIR/bootstrap/platform-gitops/gitops-operator/olm-gitops-operatorgroup.yaml" \
    -f "$SSC_DEMO_DIR/bootstrap/platform-gitops/gitops-operator/olm-gitops-subscription.yaml"
  # The demo manifest pins an older starting CSV. Let the current 4.20 catalog
  # resolve the selected channel instead of failing on that stale pin.
  oc patch subscription openshift-gitops-operator -n openshift-operators \
    --type=merge -p '{"spec":{"startingCSV":null}}' >/dev/null
  echo "GitOps operator bootstrap applied; wait for the CSV before creating the SSC app-of-apps."
}

bootstrap_pipelines() {
  require_file "$SSC_DEMO_DIR/automation/gitops/components/pipelines/Chart.yaml"
  check_login
  command -v helm >/dev/null || { echo "helm is required for --bootstrap-pipelines" >&2; exit 1; }
  oc create namespace openshift-pipelines --dry-run=client -o yaml | oc apply -f -
  # Tekton Chains is configured by the SSC app-of-apps after RHTAS exists;
  # do not start its bootstrap Job during this operator-only preparation.
  helm template ssc-pipelines "$SSC_DEMO_DIR/automation/gitops/components/pipelines" \
    --set chains.enabled=false | oc apply -f -
  echo "SSC Pipelines operator resources applied from the existing chart (Chains deferred until RHTAS)."
}

bootstrap_platform_argocd() {
  local dir="$SSC_DEMO_DIR/bootstrap/platform-gitops/gitops-platform-instance"
  require_file "$dir/namespace-platform-gitops.yaml"
  require_file "$dir/platform-gitops-argocd.yaml"
  require_file "$dir/platform-gitops-clusterrole.yaml"
  require_file "$dir/platform-gitops-clusterrolebinding.yaml"
  check_login
  oc apply \
    -f "$dir/namespace-platform-gitops.yaml" \
    -f "$dir/platform-gitops-clusterrole.yaml" \
    -f "$dir/platform-gitops-clusterrolebinding.yaml" \
    -f "$dir/platform-gitops-argocd.yaml"
  echo "SSC platform ArgoCD instance and RBAC applied. Component Applications were not applied."
}

case "$ACTION" in
  --preflight) preflight ;;
  --ensure-storage) ensure_storage ;;
  --bootstrap-gitops) bootstrap_gitops ;;
  --bootstrap-platform-argocd) bootstrap_platform_argocd ;;
  --bootstrap-pipelines) bootstrap_pipelines ;;
esac
