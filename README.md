# Anchor DRA

[![CI and release](https://github.com/anchor-dra/anchor/actions/workflows/ci.yaml/badge.svg?branch=main)](https://github.com/anchor-dra/anchor/actions/workflows/ci.yaml)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

Anchor is a Kubernetes Dynamic Resource Allocation (DRA) driver for static
carrier IPv4 endpoints. It presents the same `ResourceClaim` and pod-interface
contract on AWS and on-premises while using the placement mechanism appropriate
for each underlay.

Anchor is designed for workloads such as SIGTRAN and Diameter that need fixed,
NAT-free addresses, including SCTP multihoming with multiple addresses on
separate carrier paths. Its required NRI plugin creates ipvlan interfaces
directly without replacing the cluster's primary CNI.

> [!IMPORTANT]
> Anchor `0.2` is an alpha release and a breaking replacement for the Multus-
> based `0.1` line. It implements AWS `ip-reassign` and on-premises
> `l2-announce`. Review the [current limitations](#current-scope)
> and qualify it against your own failure and carrier-interconnect requirements.

## What Anchor provides

- One or more explicit static addresses in a DRA `ResourceClaim`.
- Scheduler filtering so the pod lands only on a node with every required
  controller-approved carrier parent.
- Atomic AWS reassignment with `AssignPrivateIpAddresses` and
  `AllowReassignment=true` when a pod moves.
- Cross-AZ AWS route repointing for independently routed `/32`s, with complete
  tagged VPC route-table convergence before ownership transfers.
- A persistent pod network plan and NRI-driven ipvlan L2 injection with direct
  address, route, and source-rule configuration through netlink.
- Durable `EndpointOwnership` records so GitOps, claim recreation, and namespace
  recreation retain the logical endpoint.
- A central, leader-elected placement controller; node plugins hold no cloud
  credentials.
- Prometheus metrics, Kubernetes events, and placement status for operations.

Anchor does not replace the primary CNI, allocate addresses from pools, create
or attach ENIs, configure TGW aggregate or external carrier routing, or
preserve a live SCTP association during node failure.

## How it works

```text
ResourceClaim -> scheduler -> ResourceSlice -> selected EC2 node
                                                 |
kubelet -> Anchor node plugin -> EndpointPlacement
                                      |
                                      v
                            Anchor controller -> EC2 ENI/IP reassignment
                                      |
                                      +-> EndpointOwnership
                                      +-> persistent PodNetworkPlan
                                      v
containerd -> Anchor NRI -> ipvlan/netlink -> pod: eth0 + sigtran-a + sigtran-b
```

Every candidate node has a pre-attached ENI for each configured carrier path.
When kubelet prepares the claim, the controller validates the allocated claim,
reserved pod, target node, subnets, tags, and ENIs before moving any address.
Anchor NRI then creates ipvlan children in the pod network namespace. The address
bound by the application is the address visible to the external peer; there is
no secondary-path NAT.

See [Architecture](docs/architecture.md) for reconciliation, ownership, routing,
and failure semantics.

## Requirements

- Kubernetes `1.34` or newer with `resource.k8s.io/v1`.
- Linux AMD64 nodes with one approved carrier parent per configured path.
- containerd NRI enabled with Anchor configured as a required plugin.
- Runtime control that guarantees primary-CNI completion before Anchor's
  selected NRI callback and blocks containers until injection succeeds.
  Anchor selects `CreateContainer` on containerd 2.2.5 and older. On 2.2.6
  and newer it injects at `RunPodSandbox` and retains an idempotent
  `CreateContainer` guard for sandboxes created during late registration.
- On AWS, one pre-attached secondary ENI per carrier path and tag-scoped IAM.
- On-premises, one provider-declared parent interface per path and explicit
  infrastructure fencing before replacing a workload from a NotReady node.
- Carrier ENIs tagged for explicit Anchor management and excluded from another
  ENI manager such as VPC CNI `ipamd`.
- AWS security groups, subnet network ACLs, VPC routes, and TGW routes that
  permit the intended peer traffic in both directions. Cross-subnet paths
  require NACL rules before broader deny rules.

The `0.2` route-repoint support target is TGW. Direct Connect and Virtual
Private Gateway topologies remain unsupported until separately qualified.

The AWS staging qualification targets Kubernetes `1.35.4`, containerd, Calico
as the primary CNI, and two independently routed SCTP paths. Cilium is expected
to follow the same extra-ENI model. VPC CNI requires carrier-ENI exclusion and
is not yet part of the `0.2` conformance run.

## Install

Choose the controller identity first:

- Kubespray or kubeadm on EC2: a tag-scoped control-plane instance profile.
- EKS: IRSA scoped to `anchor-system/anchor-controller`.

The complete policies and trust configuration are in [IAM](docs/iam.md). For a
Kubespray cluster using a control-plane instance profile:

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

For on-premises, set `--set platform=onprem` and omit AWS identity and region
values. The infrastructure-owned `DeviceClass` uses `strategy: l2-announce`,
`parentInterface`, and `subnetCidr` for each carrier path.

Label only nodes with the complete, intentionally managed carrier substrate:

```bash
kubectl label node NODE_NAME dra.anchordra.co/enabled=true
```

The manifests in [`examples/aws-ip-reassign`](examples/aws-ip-reassign) show
the admin-owned `DeviceClass`, tenant-owned `ResourceClaim`, and workload
contract. The example workload image is for the repository e2e test and must be
built or replaced before applying the example pod.

Use [Installation and upgrade](docs/install.md) for the complete install,
upgrade, endpoint-retirement, disaster-recovery, and security procedures.

## Operational safety

- One claim represents one logical endpoint and may be consumed by at most one
  pod at a time.
- One active Anchor installation may own a given address set. Active/active
  clusters sharing addresses are unsupported.
- Unknown endpoint ownership is never overwritten unless a platform administrator
  explicitly sets `dra.anchordra.co/force-steal: "true"` on the claim.
- `force-steal` is a temporary disaster-recovery action, not a normal deployment
  setting.
- Deleting a pod, claim, namespace, or Helm release does not discard durable
  endpoint ownership. This preserves endpoint identity during reconciliation.
- `EndpointOwnership` records are deliberately not garbage-collected. Permanent
  endpoint retirement is an explicit cluster-administrator procedure.

## Current scope

| Capability | `0.2` status |
|---|---|
| AWS secondary private IPv4 reassignment | Supported |
| On-prem host-parent ipvlan and L2 announcement | Supported; local kubeadm qualification |
| Same-AZ pod rescheduling | Supported |
| Multiple addresses / SCTP multihoming | Supported |
| Explicit pod gateways and routes | Supported |
| NRI ipvlan injection | Qualified on AWS and local on-prem kubeadm |
| EKS AL2023 with containerd 2.2.5 | Supported target through `CreateContainer`; EKS qualification pending |
| Kubernetes | `>=1.34` |
| Primary CNI | Calico qualified; Cilium expected; VPC CNI not yet qualified |
| Architectures | Linux AMD64 |
| Route-table `/32` repointing | Reserved; qualification required before enablement |
| Cross-AZ mobility | Not implemented |
| IP pools and dynamic allocation | Not implemented |
| IPv6 | Not implemented |
| ENI create/attach/detach | Not implemented |
| Karpenter, EKS Auto Mode, and Fargate | Not supported in `0.2` |

## Documentation

| Document | Purpose |
|---|---|
| [Architecture](docs/architecture.md) | Components, trust boundary, reconciliation, and failure behavior |
| [ADR 0001: DRA, NRI, and ipvlan](docs/adr/0001-common-dra-nri-ipvlan-architecture.md) | Accepted target architecture and parity boundary |
| [Installation and upgrade](docs/install.md) | Operator procedures and lifecycle safety |
| [IAM](docs/iam.md) | Least-privilege policy, instance profiles, and EKS IRSA |
| [AWS staging e2e](docs/e2e-aws.md) | Maintainer qualification and evidence checklist |
| [Release process](docs/releasing.md) | RC/stable workflow and published artifacts |
| [Contributing](CONTRIBUTING.md) | Development and contribution workflow |
| [Security](SECURITY.md) | Reporting vulnerabilities |

## Development

The normal local validation matches the GitHub Actions validation job:

```bash
./scripts/check.sh
make image VERSION=ci
```

`scripts/check.sh` runs Go race tests and vet, Helm lint/render checks, and the
disposable AWS e2e Terraform validation. See [AWS staging e2e](docs/e2e-aws.md)
before changing scheduling, placement, ownership, networking, or AWS mutation
behavior.

## Project

- API group and driver: `dra.anchordra.co`
- Go module: `github.com/anchor-dra/anchor`
- Container: `ghcr.io/anchor-dra/anchor`
- Helm chart: `oci://ghcr.io/anchor-dra/charts/anchor`

Anchor is licensed under the [Apache License 2.0](LICENSE).
