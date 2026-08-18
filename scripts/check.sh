#!/usr/bin/env bash
set -euo pipefail

mkdir -p .work/go-build-cache
bash -n scripts/*.sh hack/e2e/*.sh
GOCACHE=${GOCACHE:-$PWD/.work/go-build-cache} go test -race ./...
GOCACHE=${GOCACHE:-$PWD/.work/go-build-cache} go vet ./...
helm lint charts/anchor --set aws.region=eu-west-1
helm template anchor charts/anchor --namespace anchor-system \
  --set aws.region=eu-west-1 >.work/anchor-default.yaml
helm template anchor charts/anchor --namespace anchor-system \
  --set aws.region=eu-west-1 \
  --set aws.useInstanceProfile=true \
  --set 'controller.nodeSelector.node-role\.kubernetes\.io/control-plane=' \
  --set 'controller.tolerations[0].key=node-role.kubernetes.io/control-plane' \
  --set 'controller.tolerations[0].operator=Exists' \
  --set 'controller.tolerations[0].effect=NoSchedule' >.work/anchor-instance-profile.yaml
helm template anchor charts/anchor --namespace anchor-system \
  --set aws.region=eu-west-1 \
  --set-string 'controller.serviceAccount.annotations.eks\.amazonaws\.com/role-arn=arn:aws:iam::123456789012:role/anchor-controller' \
  --set-string 'controller.serviceAccount.annotations.eks\.amazonaws\.com/sts-regional-endpoints=true' >.work/anchor-irsa.yaml
grep -q 'value: "true"' .work/anchor-default.yaml
grep -q 'value: "false"' .work/anchor-instance-profile.yaml
grep -q 'node-role.kubernetes.io/control-plane' .work/anchor-instance-profile.yaml
grep -q 'eks.amazonaws.com/role-arn: arn:aws:iam::123456789012:role/anchor-controller' .work/anchor-irsa.yaml
if helm template anchor charts/anchor --namespace anchor-system \
  --set aws.region=eu-west-1 \
  --set aws.useInstanceProfile=true \
  --set aws.credentialsSecretName=anchor-aws-credentials >/dev/null 2>&1; then
  echo "instance-profile and Secret credential modes must be mutually exclusive" >&2
  exit 1
fi
terraform -chdir=hack/e2e/aws fmt -check
terraform -chdir=hack/e2e/aws validate
