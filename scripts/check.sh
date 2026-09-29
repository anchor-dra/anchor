#!/usr/bin/env bash
set -euo pipefail

mkdir -p .work
bash -n scripts/*.sh hack/e2e/*.sh
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s scripts/tests
go test -race ./...
go vet ./...
helm lint charts/anchor --set aws.region=eu-west-1
helm template anchor charts/anchor --namespace anchor-system \
  --kube-version 1.34.0 \
  --include-crds \
  --set aws.region=eu-west-1 >.work/anchor-default.yaml
helm template anchor charts/anchor --namespace anchor-system \
  --kube-version 1.34.0 \
  --set aws.region=eu-west-1 \
  --set aws.useInstanceProfile=true \
  --set 'controller.nodeSelector.node-role\.kubernetes\.io/control-plane=' \
  --set 'controller.tolerations[0].key=node-role.kubernetes.io/control-plane' \
  --set 'controller.tolerations[0].operator=Exists' \
  --set 'controller.tolerations[0].effect=NoSchedule' >.work/anchor-instance-profile.yaml
helm template anchor charts/anchor --namespace anchor-system \
  --kube-version 1.34.0 \
  --set aws.region=eu-west-1 \
  --set-string 'controller.serviceAccount.annotations.eks\.amazonaws\.com/role-arn=arn:aws:iam::123456789012:role/anchor-controller' \
  --set-string 'controller.serviceAccount.annotations.eks\.amazonaws\.com/sts-regional-endpoints=true' >.work/anchor-irsa.yaml
helm template anchor charts/anchor --namespace anchor-system \
  --kube-version 1.34.0 \
  --set platform=onprem >.work/anchor-onprem.yaml
grep -q 'value: "true"' .work/anchor-default.yaml
grep -q 'name: endpointownerships.dra.anchordra.co' .work/anchor-default.yaml
grep -q "v.hostPath.path == '/var/run'" .work/anchor-default.yaml
grep -q "v.hostPath.path == '/var/run/netns'" .work/anchor-default.yaml
grep -q "v.hostPath.path == '/var/lib'" .work/anchor-default.yaml
grep -q 'mountPropagation: HostToContainer' .work/anchor-default.yaml
grep -q 'Only the Anchor node service account may bypass the mandatory NRI plugin check' .work/anchor-default.yaml
# An allowlisted exporter may mount protected paths, but must never gain the
# separate exemption from the mandatory NRI plugin check.
helm template anchor charts/anchor --namespace anchor-system \
  --kube-version 1.34.0 --show-only templates/admission.yaml \
  --set aws.region=eu-west-1 \
  --set 'node.hostPathAllowedServiceAccounts[0].namespace=observability' \
  --set 'node.hostPathAllowedServiceAccounts[0].name=metrics-node-exporter' \
  >.work/anchor-hostpath-allowlist.yaml
grep -q 'object.metadata.namespace == "observability"' .work/anchor-hostpath-allowlist.yaml
grep -q 'object.spec.serviceAccountName == "metrics-node-exporter"' .work/anchor-hostpath-allowlist.yaml
if grep -q 'metrics-node-exporter' .work/anchor-default.yaml; then
  echo "hostPath allowlist must be empty by default" >&2
  exit 1
fi
awk '/    - expression:/ { expression++ } expression == 2' \
  .work/anchor-hostpath-allowlist.yaml >.work/anchor-nri-validation.yaml
grep -q 'object.spec.serviceAccountName == "anchor-node"' .work/anchor-nri-validation.yaml
if grep -Eq 'metrics-node-exporter|observability' .work/anchor-nri-validation.yaml; then
  echo "hostPath allowlist must not grant an NRI bypass" >&2
  exit 1
fi
for missing in namespace name; do
  if helm template anchor charts/anchor --namespace anchor-system \
    --kube-version 1.34.0 --set aws.region=eu-west-1 \
    --set 'node.hostPathAllowedServiceAccounts[0].namespace=observability' \
    --set 'node.hostPathAllowedServiceAccounts[0].name=metrics-node-exporter' \
    --set "node.hostPathAllowedServiceAccounts[0].$missing=" \
    >.work/anchor-invalid-allowlist.log 2>&1; then
    echo "hostPath allowlist must require $missing" >&2
    exit 1
  fi
  grep -q "hostPathAllowedServiceAccounts.$missing is required" .work/anchor-invalid-allowlist.log
done
grep -q 'maxUnavailable: 0' .work/anchor-default.yaml
grep -q 'maxSurge: 1' .work/anchor-default.yaml
grep -q 'value: "false"' .work/anchor-instance-profile.yaml
grep -q 'node-role.kubernetes.io/control-plane' .work/anchor-instance-profile.yaml
grep -q 'eks.amazonaws.com/role-arn: arn:aws:iam::123456789012:role/anchor-controller' .work/anchor-irsa.yaml
grep -q -- '--platform=onprem' .work/anchor-onprem.yaml
if grep -q -- '--aws-region=' .work/anchor-onprem.yaml; then
  echo "on-prem controller must not receive an AWS region" >&2
  exit 1
fi
if helm template anchor charts/anchor --namespace anchor-system \
  --kube-version 1.34.0 \
  --set aws.region=eu-west-1 \
  --set aws.useInstanceProfile=true \
  --set aws.credentialsSecretName=anchor-aws-credentials >/dev/null 2>&1; then
  echo "instance-profile and Secret credential modes must be mutually exclusive" >&2
  exit 1
fi
terraform -chdir=hack/e2e/aws fmt -check
terraform -chdir=hack/e2e/aws validate
