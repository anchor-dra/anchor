# EKS integration and qualification

Anchor targets EKS AL2023 managed node groups with operator-controlled launch
templates. Use IRSA for the controller, `nodeadm` to configure containerd NRI,
and a separate infrastructure component to provision the carrier interfaces.
Anchor does not create or attach ENIs. EKS Auto Mode and Fargate cannot satisfy
the required runtime configuration and remain unsupported.

## Recorded status

The EKS staging record dated 2026-09-14 establishes a Kubernetes 1.35.7 cluster
with three managed workers running containerd 2.2.5, healthy VPC CNI/CoreDNS/
kube-proxy/EBS CSI, DNS and encrypted gp3 read/write checks, and the AWS Load
Balancer Controller. Application namespace/offline delivery was subsequently
exercised on that cluster.

That record leaves Anchor installation and carrier workers pending. Application
delivery on EKS does not exercise Anchor's DRA, NRI or SCTP paths. The selected
AMI and VPC CNI combination therefore remain **pending Anchor qualification**.
The self-managed EC2 runtime checks do not supply that missing EKS evidence.

The 0.2.1 chart adds `node.hostPathAllowedServiceAccounts` so approved exporters
can coexist with Anchor's hostPath admission policy. This is configurable on
both EKS and self-managed clusters; it does not exempt those exporters from
the required NRI plugin check.

## Infrastructure contract

- Configure NRI and its default validator before kubelet admits workloads.
  `anchor` must be a required plugin. See [installation](install.md#containerd).
- Containerd 2.2.5 selects the conservative `CreateContainer` hook; 2.2.6 and
  newer select `RunPodSandbox` plus the late-registration guard. Hook selection
  alone does not qualify an AMI or distribution backport.
- Attach one approved ENI per carrier path before VPC CNI starts allocating
  interfaces. Exclude carrier ENIs from VPC CNI management and reserve interface
  capacity when setting the worker pod limit.
- Label and pre-taint carrier workers for Anchor readiness. Keep ordinary
  workers on the platform's normal networking path.
- Give the controller tag-scoped EC2 permissions through [IRSA](iam.md).
  Applications own ResourceClaims; infrastructure owns the DeviceClass and
  carrier topology.

## Required qualification evidence

Record the exact AMI, Kubernetes, containerd, VPC CNI, Anchor image/chart and
infrastructure revisions. On disposable carrier workers, verify:

1. Cold boot and managed-node replacement preserve ENI ordering and VPC CNI
   exclusion. Ordinary worker networking continues to work.
2. A stopped/missing Anchor plugin blocks ordinary containers. Tenant pods and
   allowlisted exporters cannot use the NRI bypass annotation.
3. The approved exporter can mount its host paths, while an unapproved identity
   is denied. Empty or incomplete allowlist entries are rejected when rendered.
4. DRA allocation, ipvlan injection and two-path SCTP traffic succeed. Failed
   injection starts no application containers and recovers without a duplicate
   sandbox or endpoint allocation.
5. Same-AZ rescheduling and managed-node replacement retain endpoint ownership
   and restore peer traffic. Failed/throttled ENI attachment fails closed.
6. Qualify each enabled placement strategy separately, including route-repoint
   routing and return traffic where used. Drain carrier workloads before cleanup.

Publish the dated results and their limitations before changing the support
matrix to EKS-qualified. The [AWS staging procedure](e2e-aws.md) provides the
workload checks; its Kubespray provisioning steps must be replaced with the
managed-node provisioning path for this run.
