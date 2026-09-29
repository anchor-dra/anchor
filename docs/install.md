# Installation and upgrade

## Requirements

- Kubernetes 1.34 or newer with `resource.k8s.io/v1`.
- A qualified containerd release with NRI and its default validator enabled.
- `anchor` configured as a required NRI plugin on Anchor candidate nodes, with
  `dra.anchordra.co/tolerate-missing-nri-plugin` as the bootstrap exception.
- Candidate nodes pre-tainted with
  `dra.anchordra.co/not-ready=true:NoSchedule` before workloads are admitted.
- One provider-approved parent for every configured path on every candidate
  node: a tagged secondary ENI on AWS or a declared host interface on-premises.
- For AWS, a controller IAM identity with EC2 describe permissions and
  tag-scoped `AssignPrivateIpAddresses`.
- Underlay security and carrier routes that allow the endpoint protocols.

Anchor 0.2 does not require Multus, NetworkAttachmentDefinitions, or secondary
CNI binaries. The runtime/primary-CNI/NRI ordering must be qualified for every
supported platform version; configuration inspection alone is insufficient.
The standard 0.2 install is qualified against containerd 2.2.6, which includes
the NRI sandbox rollback behavior from containerd PR 13399. Containerd 2.2.5
is a separately qualified compatibility target using the later
`CreateContainer` hook, which avoids relying on sandbox rollback. Other
versions and distribution backports remain unsupported until they pass the
same version-specific runtime qualification.

Anchor reads the runtime name and version from the NRI configuration handshake.
Containerd 2.2.6 and newer inject at `RunPodSandbox` and also subscribe to
`CreateContainer` as an idempotent late-registration guard. The guard covers a
sandbox created while Anchor was absent because containerd's required-plugin
validator rejects the later container operation, not sandbox creation. Version
2.2.5 and older, or an unparseable version, use only the conservative
`CreateContainer` hook. That path creates the sandbox first but returns an
error before any init or application container is created until injection
succeeds. Hook selection does not broaden the support matrix: 2.2.5 is a
compatibility target and older releases remain unsupported unless separately
qualified.

The chart admission policy reserves the bootstrap exception for the Anchor
namespace and node service account; tenant pods cannot opt out of the required
plugin check. The node pod also mounts the host's `/var/run/netns` directory
read-only with host-to-container mount propagation so NRI can enter the network
namespace created by the primary CNI.

## Containerd

The required configuration is equivalent to:

```toml
[plugins."io.containerd.nri.v1.nri"]
  disable = false
  [plugins."io.containerd.nri.v1.nri".default_validator]
    enable = true
    required_plugins = ["anchor"]
    tolerate_missing_plugins_annotation = "dra.anchordra.co/tolerate-missing-nri-plugin"
```

Deliver this configuration through the platform's node-provisioning path so it
is in place before kubelet admits any workload; a node must never boot with
the required-plugin check absent. Restarting containerd is a disruptive node
operation, so do not edit live nodes in place: provision replacement nodes
with the new bootstrap configuration and drain the old ones.

- Immutable or managed node images: embed the configuration in the node
  bootstrap mechanism so it applies on first boot. On EKS AL2023 managed node
  groups this is `nodeadm` `NodeConfig` `spec.containerd.config` in the launch
  template user data; apply it by rolling the node group. This configures NRI
  but does not replace or patch the AMI's containerd binary. Use an EKS
  optimized AMI containing containerd 2.2.6 or a proven backport for the
  standard path. Stock EKS AL2023 AMIs containing 2.2.5 are a supported target
  through the automatically selected `CreateContainer` compatibility path; no
  custom AMI is required for version selection. The exact AMI package and EKS
  integration still need the platform qualification below before production
  rollout. A custom EKS AL2023 AMI is another independently qualified option.
- Configuration-managed nodes (for example Kubespray): deliver the file with
  the cluster's configuration management and restart containerd inside a
  drained maintenance window.
- Platforms that expose no containerd configuration (for example EKS Auto
  Mode and Fargate) cannot satisfy the required-plugin contract and are
  unsupported.

For example, the EKS AL2023 `NodeConfig` fragment is:

```yaml
apiVersion: node.eks.aws/v1alpha1
kind: NodeConfig
spec:
  containerd:
    config: |
      [plugins."io.containerd.nri.v1.nri".default_validator]
        enable = true
        required_plugins = ["anchor"]
        tolerate_missing_plugins_annotation = "dra.anchordra.co/tolerate-missing-nri-plugin"
```

Stock EKS AL2023 with containerd 2.2.5 is supported by Anchor's runtime
selection and uses `CreateContainer`; it does not require the sandbox rollback
fix. Before production rollout, qualify the selected AMI's exact package plus
`nodeadm`, required-plugin enforcement, primary-CNI ordering, and cold restart
behavior. The self-managed EC2 staging run in this repository qualifies the
runtime workaround. The separate EKS infrastructure baseline has also been
deployed, but its recorded checks do not qualify Anchor's EKS dataplane. See
[EKS integration and qualification](eks.md) for the evidence boundary and
remaining tests.

Approved infrastructure agents that need protected host paths can be listed
explicitly in the 0.2.1 chart:

```yaml
node:
  hostPathAllowedServiceAccounts:
    - namespace: observability
      name: metrics-node-exporter
```

The default list is empty. Each entry requires both namespace and service-account
name. This exception grants hostPath access only; the required NRI plugin bypass
remains restricted to Anchor's own node service account.

After provisioning, verify on a candidate node that an ordinary pod cannot
start while the Anchor node plugin is stopped.

## Install

Label and pre-taint only nodes with the complete carrier substrate:

```bash
kubectl label node NODE_NAME dra.anchordra.co/enabled=true
kubectl taint node NODE_NAME dra.anchordra.co/not-ready=true:NoSchedule
kubectl label namespace anchor-system pod-security.kubernetes.io/enforce=privileged --overwrite
```

For Kubespray on EC2, attach the tag-scoped policy to the control-plane
instance profile and install:

```bash
helm upgrade --install anchor oci://ghcr.io/anchor-dra/charts/anchor \
  --version 0.2.0 \
  --namespace anchor-system --create-namespace \
  --set aws.region=eu-west-1 \
  --set aws.useInstanceProfile=true \
  --set 'controller.nodeSelector.node-role\.kubernetes\.io/control-plane=' \
  --set 'controller.tolerations[0].key=node-role.kubernetes.io/control-plane' \
  --set 'controller.tolerations[0].operator=Exists' \
  --set 'controller.tolerations[0].effect=NoSchedule'
```

For EKS, leave `aws.useInstanceProfile=false` and annotate only the controller
service account with the IRSA role. The node service account must never receive
cloud credentials.

For on-premises, install without AWS configuration:

```bash
helm upgrade --install anchor oci://ghcr.io/anchor-dra/charts/anchor \
  --version 0.2.0 \
  --namespace anchor-system --create-namespace \
  --set platform=onprem
```

Use `strategy: l2-announce` and declare `parentInterface` plus `subnetCidr` on
every `DeviceClass` path. Anchor verifies the parent and emits gratuitous ARP
after injection. Before replacing an endpoint from a NotReady node, the
infrastructure provider must power- or hypervisor-fence it and then annotate
the old Node `dra.anchordra.co/fenced=true`. After the Node is NotReady, force
delete the stale pod object so Kubernetes can release its DRA reservation and
schedule the replacement. Remove the annotation after the node is safely
rebuilt or rejoins. A Kubernetes NotReady condition alone is not fencing
evidence, and force deletion must never precede provider fencing.

Apply the `DeviceClass`, `ResourceClaim`, and pod under
`examples/aws-ip-reassign`. Each class path must declare `interfaceName` and a
unique `routingTable`; `gateway` and `routes` are configured together. The pod
manifest contains no networking annotation.

AWS route-repoint is explicitly enabled and advertised through the chart's
`aws.enabledStrategies` and `aws.advertisedStrategies` lists. Its paths use an
administrator-owned `subnets` list with one AZ-local gateway per allowed
subnet and a complete `routeTableIds` set. Anchor changes only exact endpoint
`/32` routes in those tagged VPC route tables. TGW aggregate routes,
attachments, propagation, return routes, and carrier advertisements remain
infrastructure-owned. The 0.2 support target is TGW; DX and VGW topologies are
unsupported until they complete separate qualification.

DRA freezes DeviceClass configuration into a claim allocation. When an
allowed subnet or managed route table changes, reallocate claims one endpoint
at a time. Recreating a pod is sufficient for template-backed claims; a
standalone claim must be scaled down, deleted and recreated under the same
logical name, and then brought back up. Do not activate a new ingress path
until every placement reports `ConfigurationCurrent=True` and every required
route table is `Converged`.

## Upgrade from 0.1

0.2 is a coordinated breaking cutover, not a rolling injector migration:

1. Upgrade the cluster runtime configuration and install the bootstrap taint.
2. Remove or replace service manifests that carry Multus annotations.
3. Apply the 0.2 CRDs, DeviceClasses, and chart.
4. Recreate endpoint workloads so DRA writes new persistent plans.
5. Remove obsolete claim-specific NADs only after no 0.1 workload uses them.

Do not run 0.1 and 0.2 Anchor node plugins on the same candidate node.
`EndpointOwnership` and existing endpoint placement remain non-destructive
through the cutover.

## Operations

Watch `EndpointPlacement.status.injection`, `NetworkInjectionFailed` events,
`anchor_network_injections_total`,
`anchor_network_injection_retries_total`, and
`anchor_network_injection_failure_age_seconds`. If injection remains failed,
the periodic reconciler keeps attempting idempotent repair. Correct the host
parent or plan input if necessary, drain the node, or delete the pod to trigger
normal rescheduling. A single workload failure does not withdraw unrelated
node capacity. Anchor does not automatically roll back cloud placement or
delete the pod.

Deleting a pod, claim, namespace, or Helm release does not retire an endpoint.
After fencing all live claims, an administrator may explicitly clean up the AWS
address and delete its `EndpointOwnership` record. Never infer retirement from
an absent claim.
