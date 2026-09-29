# Anchor 0.2.1 chart qualification

Date: 2026-09-29. Scope: the hostPath service-account allowlist and its isolation
from the NRI bypass rule. Controller and node implementation code is unchanged
from 0.2.0.

Local verification passed: Go race tests, `go vet`, Helm lint and AWS/on-prem
renders, Terraform format/validation, and the Linux AMD64 container image build.
The chart checks cover the empty default allowlist, configured identities,
required namespace/name fields and unchanged NRI bypass permissions.

The rendered chart admission policy was installed in a disposable Kubernetes
1.34.0 kind cluster. The API server reported no CEL type-check warnings. Seven
server-side Pod dry runs passed:

| Case | Result |
| --- | --- |
| Approved exporter with protected hostPath | Allowed |
| Unapproved service account with protected hostPath | Denied |
| Approved account name in a different namespace | Denied |
| Approved exporter requesting the NRI bypass | Denied |
| Ordinary pod requesting the NRI bypass | Denied |
| Anchor node service account with hostPath and bootstrap bypass | Allowed |
| Ordinary pod without protected paths or bypass | Allowed |

No workload Pods were created by these admission checks. They qualify the
chart's permission change, not the NRI runtime or carrier dataplane. EKS status
and the remaining platform checks are recorded in [EKS integration](eks.md).
The release workflow must also pass validation and publish an RC before the
identical tree is promoted to main.
