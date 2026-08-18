# Installation and upgrade

## Requirements

- Kubernetes 1.34 or newer with `resource.k8s.io/v1`.
- Multus and the NetworkAttachmentDefinition CRD.
- `ipvlan`, `static`, and `sbr` binaries in the node CNI binary directory.
- One pre-attached, tagged secondary ENI for every configured path on every
  candidate node. All ENIs for an endpoint must be in the same AZ as the node.
- A controller IAM identity with EC2 describe permissions and tag-scoped
  `AssignPrivateIpAddresses`.
- Security groups and subnet network ACLs that allow the endpoint protocol in
  both directions. Cross-subnet paths require NACL allows before any broader
  deny rule for the other carrier subnet.
- Exclusive control of every configured static address by one active Anchor
  installation. Clusters sharing a subnet must use non-overlapping addresses.

Calico and Cilium do not normally manage extra ENIs. When VPC CNI is primary,
exclude carrier ENIs from ipamd before installing Anchor. Anchor 0.1 does not
advertise VPC-CNI compatibility as tested.

Upgrade and validate the Kubernetes layer before installing Anchor; Anchor does
not enable DRA feature gates or upgrade Kubernetes.

## Install

Choose one controller credential source. See [the IAM guide](iam.md) for the
required policy and complete Kubespray and EKS examples.

### Kubespray on EC2

Attach the tag-scoped policy to the control-plane instance profile and keep the
controller on control-plane nodes:

```bash
helm upgrade --install anchor oci://ghcr.io/anchor-dra/charts/anchor \
  --version 0.1.0 \
  --namespace anchor-system --create-namespace \
  --set aws.region=eu-west-1 \
  --set aws.useInstanceProfile=true \
  --set 'controller.nodeSelector.node-role\.kubernetes\.io/control-plane=' \
  --set 'controller.tolerations[0].key=node-role.kubernetes.io/control-plane' \
  --set 'controller.tolerations[0].operator=Exists' \
  --set 'controller.tolerations[0].effect=NoSchedule'
```

The control-plane instances must expose IMDSv2 to pod networking with response
hop limit `2`. This mode shares the node identity, so do not schedule tenant
workloads on those nodes.

### EKS with IRSA

Create the IAM OIDC provider and role described in the IAM guide, then annotate
only the controller service account:

```bash
helm upgrade --install anchor oci://ghcr.io/anchor-dra/charts/anchor \
  --version 0.1.0 \
  --namespace anchor-system --create-namespace \
  --set aws.region=eu-west-1 \
  --set-string 'controller.serviceAccount.annotations.eks\.amazonaws\.com/role-arn=arn:aws:iam::ACCOUNT_ID:role/anchor-controller' \
  --set-string 'controller.serviceAccount.annotations.eks\.amazonaws\.com/sts-regional-endpoints=true'
```

Leave `aws.useInstanceProfile=false` and `aws.credentialsSecretName` empty for
IRSA. EKS injects the projected web-identity token from the annotation.

### Temporary test credentials

For a disposable e2e environment only, create `anchor-aws-credentials` with
`hack/e2e/assume-role-secret.sh` and set
`aws.credentialsSecretName=anchor-aws-credentials`. Never combine the Secret
with `aws.useInstanceProfile=true`.

`aws.apiQPS` and `aws.apiBurst` define one shared limit across all controller
EC2 discovery and mutation calls. Keep the conservative defaults unless the
account owner has assigned a specific API budget to Anchor.
`controller.reconcileInterval` controls pending-placement retries;
`controller.inventoryInterval` and `controller.driftInterval` default to 60
seconds so Ready endpoints do not consume the EC2 budget every retry tick.
Monitor `anchor_reconcile_duration_seconds` by its `placement`, `inventory`,
and `drift` labels when validating cluster capacity.

Label only nodes whose carrier substrate is intentionally managed:

```bash
kubectl label node NODE_NAME dra.anchordra.co/enabled=true
```

Apply a DeviceClass, a standalone ResourceClaim, and a workload from
`examples/aws-ip-reassign`. A replicated workload must not share one endpoint
claim. One claim is one logical endpoint and may have at most one active pod.

Routing is configured by the platform administrator on each DeviceClass path.
Set `gateway` together with one or more `routes`; Anchor rejects partial pairs,
invalid IPv4 values, and gateways outside the discovered subnet. The `sbr`
plugin uses that gateway as the source-specific default for traffic explicitly
bound to the carrier address; configured routes also record the intended peer
CIDRs. An explicit `0.0.0.0/0` route is therefore unnecessary. Omitting both
fields leaves the interface reachable only within its subnet. Anchor does not
configure or advertise the corresponding DX, TGW, or VPC routes outside the
pod.

## Upgrade

1. Pull and unpack the target chart, then apply its CRDs before upgrading. Helm
   installs CRDs but deliberately does not upgrade them:

   ```bash
   helm pull oci://ghcr.io/anchor-dra/charts/anchor \
     --version TARGET_VERSION --untar --untardir /tmp
   kubectl apply -f /tmp/anchor/crds
   ```

2. Keep the controller running and repeat the applicable install command with
   `--version TARGET_VERSION`. The chart selects the matching immutable image
   tag through its `appVersion`.
3. Wait for both controller replicas and the node DaemonSet rollout.
4. Confirm existing EndpointPlacements remain `Ready`, EndpointOwnership
   records contain the expected ENIs, and ResourceSlices still advertise the
   expected slots.
5. Restart endpoint workloads only after that validation.

The CRDs, EndpointOwnership records, and AWS placements survive
`helm uninstall`. EndpointPlacements and NADs are ephemeral and are deleted
with their ResourceClaims; durable ownership is not. Anchor never
garbage-collects EndpointOwnership records: permanent retirement is an explicit
cluster-administrator action.

## Release an endpoint permanently

Deleting a pod, claim, or namespace does not relinquish a carrier endpoint.
This is what allows GitOps prune/apply and namespace recreation to retain the
same static addresses. To release an endpoint intentionally:

1. Delete its workload and ResourceClaim and wait for their EndpointPlacement
   and NADs to disappear.
2. Record the address and `status.eniId`, and verify no live claim still uses
   the ownership:

   ```bash
   kubectl get endpointownerships.dra.anchordra.co
   kubectl get endpointownerships.dra.anchordra.co OWNERSHIP_NAME -o yaml
   ```

3. Delete the ownership record as a cluster administrator:

   ```bash
   kubectl delete endpointownerships.dra.anchordra.co OWNERSHIP_NAME
   ```

4. If the address must return to the subnet, unassign it explicitly from the
   recorded ENI. Anchor deliberately does not do this during claim deletion or
   Helm uninstall.

If a live claim still exists, the controller recreates its missing ownership
record. A later claim with a different namespace/name cannot take an address
that still has an ownership record without `force-steal`.

## Disaster recovery and cluster replacement

Include cluster-scoped `EndpointOwnership` resources in the Kubernetes or etcd
backup used for Anchor disaster recovery. They are Anchor's only durable record
of the logical claim owner and last successful ENI placement; AWS does not hold
an equivalent Anchor ownership marker.

Before bringing up a replacement cluster against the same address range:

1. Stop the old Anchor controller or revoke its AWS mutation permission. Do not
   run two active controllers with force takeover enabled.
2. Restore the ownership records when available and confirm their claim names,
   addresses, and ENIs before creating replacement claims.
3. If the records cannot be restored, inspect the current AWS ENI assignment,
   confirm the old controller is fenced, and temporarily authorize the intended
   replacement claim:

   ```bash
   kubectl annotate resourceclaim -n NAMESPACE CLAIM_NAME \
     dra.anchordra.co/force-steal=true --overwrite
   ```

4. Wait for the replacement `EndpointPlacement` to become `Ready`, verify the
   address on the expected ENI and pod interface, then immediately remove the
   takeover permission:

   ```bash
   kubectl annotate resourceclaim -n NAMESPACE CLAIM_NAME \
     dra.anchordra.co/force-steal-
   ```

Without `force-steal`, a second cluster safely refuses an address assigned to an
unknown ENI. With it left enabled on two clusters, each cluster can undo the
other's placement during reconciliation. Anchor 0.1 therefore does not support
active/active or automatically coordinated active/standby clusters sharing an
address range.

## Security boundary

The controller runs non-root with no Linux capabilities. IMDS is disabled by
default and enabled only by the explicit Kubespray instance-profile setting.
The node plugin receives no AWS Secret or IRSA annotation. It runs in the host
network namespace with only `NET_ADMIN`, which is needed to make hot-attached
parent links usable, and has `allowPrivilegeEscalation: false`.

Do not grant tenants access to `EndpointPlacement`, `EndpointOwnership`, or
`AnchorNodeInventory`; they are driver-internal APIs. Namespace users need only
create the ResourceClaim and workload objects allowed by the platform. The
controller independently derives every AWS mutation from the allocated claim,
its reserved Pod, and controller inventory, but access to the force-steal claim
annotation should still be restricted to the platform's endpoint transfer
procedure.
