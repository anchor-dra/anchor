#!/usr/bin/env bash
set -euo pipefail

mkdir -p .work/go-build-cache
bash -n scripts/*.sh hack/e2e/*.sh
GOCACHE=${GOCACHE:-$PWD/.work/go-build-cache} go test -race ./...
GOCACHE=${GOCACHE:-$PWD/.work/go-build-cache} go vet ./...
helm lint charts/anchor --set aws.region=eu-west-1
helm template anchor charts/anchor --namespace anchor-system --set aws.region=eu-west-1 >/dev/null
terraform -chdir=hack/e2e/aws fmt -check
terraform -chdir=hack/e2e/aws validate
