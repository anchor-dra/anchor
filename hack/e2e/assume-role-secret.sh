#!/usr/bin/env bash
set -euo pipefail

role_arn=${1:?usage: $0 <role-arn> [aws-profile] [namespace]}
aws_profile=${2:-opsw_admin_rcs}
namespace=${3:-anchor-system}
work_dir=${ANCHOR_WORK_DIR:-.work}
credentials_file="$work_dir/controller-credentials.env"
kubectl_args=()

if [[ -n "${KUBECTL_CONTEXT:-}" ]]; then
  kubectl_args+=(--context "$KUBECTL_CONTEXT")
fi

mkdir -p "$work_dir"
umask 077
aws --profile "$aws_profile" sts assume-role \
  --role-arn "$role_arn" \
  --role-session-name anchor-e2e \
  --output json >"$work_dir/assume-role.json"

jq -r '
  "AWS_ACCESS_KEY_ID=" + .Credentials.AccessKeyId,
  "AWS_SECRET_ACCESS_KEY=" + .Credentials.SecretAccessKey,
  "AWS_SESSION_TOKEN=" + .Credentials.SessionToken
' "$work_dir/assume-role.json" >"$credentials_file"

kubectl "${kubectl_args[@]}" create namespace "$namespace" --dry-run=client -o yaml | kubectl "${kubectl_args[@]}" apply -f -
kubectl "${kubectl_args[@]}" -n "$namespace" create secret generic anchor-aws-credentials \
  --from-env-file="$credentials_file" \
  --dry-run=client -o yaml | kubectl "${kubectl_args[@]}" apply -f -

echo "created $namespace/anchor-aws-credentials from short-lived role credentials"
