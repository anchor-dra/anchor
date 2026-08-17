# Installation and upgrade

## Requirements

- Kubernetes 1.34 or newer with `resource.k8s.io/v1`.
- Multus and the NetworkAttachmentDefinition CRD.
- `ipvlan`, `static`, and `sbr` binaries in the node CNI binary directory.
- One pre-attached, tagged secondary ENI for every configured path on every
  candidate node. All ENIs for an endpoint must be in the same AZ as the node.
- A controller IAM identity with EC2 describe permissions and tag-scoped
  `AssignPrivateIpAddresses`.

Calico and Cilium do not normally manage extra ENIs. When VPC CNI is primary,
exclude carrier ENIs from ipamd before installing Anchor. Anchor 0.1 does not
advertise VPC-CNI compatibility as tested.

Moving a cluster from Kubernetes 1.33 to 1.34 is a breaking platform upgrade.
For the Iris platform, Kubernetes 1.33 remains on the 0.x infrastructure
release line and Kubernetes 1.34 starts the 1.x line. Upgrade and validate the
Kubernetes layer before installing Anchor; Anchor does not upgrade Kubernetes.

## Install

Create the controller credential Secret using the deployment platform's normal
OIDC mechanism, or for the staging proof use `hack/e2e/assume-role-secret.sh`.
Then install the chart:

```bash
helm upgrade --install anchor charts/anchor \
  --namespace anchor-system --create-namespace \
  --set aws.region=eu-west-1 \
  --set aws.credentialsSecretName=anchor-aws-credentials \
  --set image.repository=anchor \
  --set image.tag=0.1.0 \
  --set image.pullPolicy=IfNotPresent
```

`aws.apiQPS` and `aws.apiBurst` define one shared limit across all controller
EC2 discovery and mutation calls. Keep the conservative defaults unless the
account owner has assigned a specific API budget to Anchor.

Label only nodes whose carrier substrate is intentionally managed:

```bash
kubectl label node NODE_NAME anchor.dra.example.com/enabled=true
```

Apply a DeviceClass, a standalone ResourceClaim, and a workload from
`examples/aws-ip-reassign`. A replicated workload must not share one endpoint
claim. One claim is one logical endpoint and may have at most one active pod.

## Upgrade

1. Keep the controller running and upgrade the chart with the new immutable
   image tag.
2. Wait for both controller replicas and the node DaemonSet rollout.
3. Confirm existing EndpointPlacements remain `Ready` and ResourceSlices still
   advertise the expected slots.
4. Restart endpoint workloads only after that validation.

The CRDs and AWS placements survive `helm uninstall`. Uninstalling the chart
does not unassign carrier addresses. Inspect `EndpointPlacement` objects and
perform explicit cleanup before removing ENIs.

## Security boundary

The controller runs non-root with no Linux capabilities and disables IMDS
credential fallback. The node plugin receives no AWS Secret. It runs in the
host network namespace with only `NET_ADMIN`, which is needed to make hot-
attached parent links usable, and has `allowPrivilegeEscalation: false`.
