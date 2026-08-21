# ADR 0001: Use DRA and NRI with an ipvlan dataplane

- Status: Accepted
- Date: 2026-08-20
- Decision owners: Anchor maintainers
- Scope: Anchor `0.2` and the shared on-premises/AWS carrier-networking architecture

## Context

Anchor `0.1` artifacts were published (the `0.1.0` Helm chart and container
images on `ghcr.io/anchor-dra`, and the corresponding git tags and releases),
and `iris-infra-kubernetes` and the service layer have releases that consume
the Multus-era contract. Anchor has not run in production, but the published
line is retained as immutable release history. Its `0.1` implementation uses a
Kubernetes DRA `ResourceClaim` to schedule and place an AWS endpoint, then uses
Multus, a generated per-claim `NetworkAttachmentDefinition` (NAD), and the
`ipvlan`, `static`, and `sbr` CNI plugins to create the pod interfaces.

That implementation describes one attachment twice:

1. The `ResourceClaim` selects and reserves the endpoint.
2. A pod annotation names generated NADs that describe the same paths and
   addresses.

The duplicate contract creates name coupling, ordering and garbage-collection
work, and an additional qualification surface. The breaking `0.2` release is
the boundary at which this duplicate contract is removed.

The wider platform has two layers:

- `iris-infra-kubernetes` owns cluster and carrier-network infrastructure.
- The service layer deploys workloads such as SIGTRAN and Diameter.

The service layer is intended to behave the same on-premises and on AWS. The
underlays cannot be identical: on-premises uses VLAN-backed L2 domains, while
AWS requires EC2 control-plane operations because GARP cannot move an AWS
address. Parity therefore has to be defined above the underlay.

## Decision drivers

- One workload attachment contract, with no environment-specific pod
  annotations or Helm branches.
- The same pod-visible interface names, addressing structure, routing behavior,
  failure behavior, status, and diagnostics on-premises and on AWS.
- Preserve ipvlan density and disposable child-interface cleanup.
- Keep AWS credentials and mutations in the central controller.
- Fail closed: a workload must not start without every claimed carrier path.
- Recover by reconciling durable desired state, not by assuming lifecycle hooks
  are delivered once or in order.
- Avoid supporting two injectors and two qualification matrices.
- Allow a future direct-device injector without exposing the implementation in
  the service-layer API.

## Decision

Anchor `0.2` is rewritten around a common DRA and NRI lifecycle. It does not
ship or retain the Multus implementation.

### Version and release hygiene

The rewrite is released as `0.2`. Published `0.1` git tags, releases, charts,
and images remain available as historical artifacts and continue to describe
only the Multus implementation. They are not overwritten or republished.

`0.2` is an intentionally breaking pre-production release. There is no mixed
`0.1`/`0.2` mode and no in-place migration of claim-specific NADs. The
infrastructure and service-layer releases that adopt this ADR must cut over
together, remove the Multus-era workload contract, and pin Anchor `0.2`.

### Parity boundary

The following table defines the compatibility boundary between the service
layer and infrastructure:

| Concern | On-premises | AWS | Must match |
|---|---|---|---|
| Workload attachment | `ResourceClaim` | `ResourceClaim` | Yes |
| Interface names | Profile-defined, for example `sigtran-a` and `sigtran-b` | Same | Yes |
| Carrier address structure | One address per path | Same | Yes |
| Source routing | Per-path rules and tables | Same behavior | Yes |
| Readiness and failures | Anchor status and events | Same | Yes |
| Parent interface | VLAN-backed host link | Tagged carrier ENI | No |
| Endpoint steering | L2 ownership and announcement | EC2 IP reassignment or route repointing | No |

Exact parent devices, MAC addresses, and steering APIs are infrastructure
details. They must not leak into the workload contract.

### Workload contract

The `ResourceClaim` is the sole service-layer attachment contract. A workload
does not contain:

- `k8s.v1.cni.cncf.io/networks` annotations;
- NAD names;
- CNI plugin configuration; or
- environment-specific interface-injection settings.

Infrastructure-owned `DeviceClass` configuration defines the path profile,
parent selection, gateways, peer routes, and steering strategy. Tenant-owned
claim configuration supplies the endpoint addresses. The same class names and
claim shape should be installed in both environments where their semantics
match.

Both direct pod claim references and claims generated from a
`ResourceClaimTemplate` are supported. For template-backed references, the NRI
plugin resolves the generated claim name through
`Pod.status.resourceClaimStatuses`; a missing or inconsistent mapping fails the
sandbox closed. Durable ownership for a template-backed claim is keyed by its
namespace, reserving Pod name, and pod claim alias rather than the generated
ResourceClaim suffix. This keeps ownership stable across recreation of the
same workload while preserving the generated claim name and UID for placement,
events, plan storage, and garbage collection.

### Common component model

```text
                         service layer
                              |
                       ResourceClaim
                              |
                    common Anchor DRA driver
                              |
              +---------------+---------------+
              |                               |
       InventoryProvider              PlacementStrategy
              |                               |
       +------+-------+            +----------+----------+
       |              |            |          |          |
    On-premises      AWS       L2Announce IPReassign RouteRepoint
    VLAN inventory   ENI inventory
              \                               /
               +-----------------------------+
                              |
                    NRI IPvlanInjector
                              |
                   pod network namespace
```

The initial internal seams are:

```go
type InventoryProvider interface {
	Discover(context.Context) ([]EndpointDevice, error)
}

type PlacementStrategy interface {
	Place(context.Context, EndpointPlacement) error
	Verify(context.Context, EndpointPlacement) error
}

type LinkInjector interface {
	Inject(context.Context, PodNetworkPlan, NetworkNamespace) error
	Reconcile(context.Context) error
}
```

These are architectural boundaries, not a commitment to public Go interfaces
with these exact signatures.

### DRA device model

One DRA device represents one complete endpoint path-bundle slot on one node,
not an individual address, interface, ENI, or path. A dual-path endpoint is
therefore allocated atomically as one device and cannot be split across nodes.

The node component publishes node-local `ResourceSlice` objects. A device is
published only when every path required by its profile has a usable parent and
the node can support the profile's injection contract. Its attributes contain
the infrastructure identities and topology needed for controller-side
validation and scheduler filtering; those attributes remain below the
service-layer ABI.

For each profile, advertised capacity is the lower of:

- the administrator-configured `slotsPerNode`; and
- the capacity that can be satisfied across all required paths.

If inventory, NRI health, or a required parent becomes unavailable, Anchor
withdraws unallocated devices from subsequent `ResourceSlice` publications.
Existing allocations are never reassigned merely by changing advertised
capacity. The controller continues to derive and verify the selected node,
device, profile, and pod UID from the allocated claim before placement.

### Initial link injector

`IPvlanInjector` is the only `0.2` injector. At the qualified NRI sandbox
lifecycle point it:

1. Resolves the persisted plan by pod UID.
2. Verifies the plan matches the allocated claim and sandbox.
3. Creates one ipvlan L2 child for each path from the discovered host parent.
4. Moves the child into the pod network namespace and assigns its stable name.
5. Configures addresses, link-scope gateway reachability, routes, and
   source-specific rules directly through netlink.
6. Verifies the resulting namespace state before returning success.

The intended ordering is: DRA preparation and cloud placement, primary CNI
creation of `eth0`, Anchor injection of the claimed secondary carrier
interfaces, then init and application container start. This ordering is a
release qualification requirement rather than an assumption about every
runtime version. Anchor reads the runtime name and version from the NRI
configuration handshake and selects its event subscription before processing
workloads. Containerd 2.2.6 and newer use `RunPodSandbox` as the primary hook
and retain `CreateContainer` as an idempotent late-registration guard because
the required-plugin validator permits sandbox creation before rejecting the
container operation. Containerd 2.2.5 and older, plus unparseable versions,
use only `CreateContainer` so an injection error cannot enter the affected
sandbox rollback path. Unsupported versions remain unsupported even though
they receive the conservative hook. Both callbacks preserve the same
before-container-start guarantee.

Anchor does not invoke Multus or the `ipvlan`, `static`, or `sbr` CNI binaries.
It owns the equivalent local netlink behavior and its tests.

The parent remains in the host namespace. The disposable ipvlan child is lost
with the pod namespace, which keeps local teardown recovery lower-risk than
moving a physical ENI into the pod.

### Environment adapters

The architecture defines these placement strategies:

- `l2-announce`: on-premises ownership fencing followed by the required L2
  announcement. It is not a no-op; Anchor must prevent two live owners and
  reconcile stale ownership.
- `ip-reassign`: assign each subnet-local secondary private IPv4 address to the
  selected AWS parent ENI with `AllowReassignment=true`.
- `route-repoint`: point every managed carrier-address route at the selected
  AWS parent ENI and verify all required route tables.

`ip-reassign` is enabled for AWS and `l2-announce` is enabled for on-premises.
The on-prem adapter inventories administrator-declared host parents, fences
logical ownership through `EndpointOwnership`, requires explicit provider
fencing before a move away from a NotReady node, and sends gratuitous ARP after
namespace injection. `route-repoint` remains qualification-gated and is not
published in DRA inventory by default. Operators first enable its executor,
complete the TGW qualification and preflight, and only then enable
advertisement. A pod
therefore cannot schedule on a strategy that its selected controller cannot
execute.

Strategies share claim validation, durable `EndpointOwnership`, force-takeover
policy, batching, rate limiting, drift detection, status, and events. Only the
environment-specific discovery and steering operations differ.

Strategies must be selected explicitly. Anchor will not silently default to a
placement strategy.

### Route repointing

For `route-repoint`, the carrier address is an independently routed `/32`; it
does not have to be an AWS-assigned secondary address on the parent ENI. The
selected ENI is the AWS route target, while the ipvlan child owns the address
inside the pod.

The route-repoint design requires a dataplane qualification before release. It
must prove:

- inbound delivery from an AWS `/32` route targeting a shared parent ENI to the
  correct ipvlan child;
- outbound traffic with the carrier `/32` as source;
- isolation between multiple ipvlan children on the same parent;
- the required underlay address and on-link gateway model;
- source/destination-check, security-group, NACL, and TGW behavior;
- dual-path SCTP behavior; and
- same-AZ and cross-AZ route movement.

This qualification determines the exact netlink plan, not whether Anchor falls
back to Multus. If shared-parent ipvlan cannot satisfy the AWS dataplane, the
exception must be recorded in a new ADR. A future `DirectENIInjector` may then
be considered without changing the ResourceClaim or pod-visible ABI.

Each path declares the complete infrastructure-owned set of route tables that
must agree on the `/32` target. AWS cannot update several route tables
transactionally. Anchor therefore records and verifies per-table progress,
retries partial updates, and does not transfer durable endpoint ownership until
the required set has converged. For multihomed endpoints, it places and verifies
one path at a time so an already healthy path is not needlessly disrupted.

The managed set contains only VPC route tables whose exact endpoint `/32`
lookup must select the carrier ENI, including TGW attachment-subnet or
inspection-subnet tables where applicable. TGW route-table entries for the
carrier aggregate, peer and return routes, attachment associations and
propagation, and DX/VGW/BGP advertisements are stable infrastructure-owned
covering routes. Qualification inventories every lookup hop and classifies it
as either a listed Anchor-managed `/32` lookup or a stable aggregate lookup.

Route-repoint paths list every allowed AZ-local carrier subnet and its gateway.
Inventory selects an ENI only when both its tags and subnet match; placement
persists only the resolved subnet and gateway. Editing a DeviceClass does not
rewrite DRA's allocated class configuration. Managed-set changes therefore use
one-at-a-time claim reallocation and expose a `ConfigurationCurrent` condition;
a pod restart alone is insufficient for a standalone claim.

Anchor provides endpoint reachability after rescheduling; preserving a live
SCTP association across pod or node failure is not a `0.2` guarantee. Cloud
placement may precede local NRI injection. If injection then fails, the
carrier identity remains directed at a sandbox that is not serving traffic;
this blackhole is not inherently time-bounded.

Anchor keeps the workload fail-closed and retries reconciliation. It publishes
a `NetworkInjectionFailed` warning event, a failure condition with the first
failure time and last error, retry and failure-age metrics, and removes the
node's unallocated devices from advertised capacity when injector health is
lost. For `0.2`, Anchor does not automatically roll cloud placement back,
delete the pod, or transfer durable ownership after an arbitrary retry count.
Persistent failure requires an operator to correct the node or delete the pod
or drain the node so normal scheduling and placement can run again. These
signals and the operator action are part of the runbook and alerting contract.
The NRI callback remains local and does not perform AWS calls.

Route capacity is discovered from the target account and route tables by a
mandatory installation and scaling preflight. The long-running controller does
not receive Service Quotas access. A runtime route-limit error is isolated to
the affected placement and reported through status, events, and metrics.

### Trust boundary

The existing trust boundary remains:

- The central, leader-elected controller is the only component with cloud
  credentials.
- The node component is privileged for host networking and NRI access but has
  no AWS credentials.
- The node service account cannot update Node objects. The leader-elected
  controller owns cluster-wide Node update permission and reconciles the
  Anchor readiness taint from node-written `AnchorNodeInventory.status`.
- The controller reconstructs and validates placement from the allocated
  claim, reserved pod UID, selected node, controller-written inventory, and
  administrator-owned class configuration before any cloud mutation.
- Node-provided interface names are matched to controller-approved devices by
  stable identity such as MAC and ENI ID.

NRI access is security-sensitive. The NRI socket is mounted only into the
Anchor node DaemonSet. Because the DaemonSet requires host networking,
host-path access, and elevated network capabilities, its dedicated namespace
is explicitly admitted at the required Pod Security Admission level; service
workloads cannot create pods there or use the Anchor service account. A
`ValidatingAdmissionPolicy` or the cluster's Kyverno/Gatekeeper equivalent
denies the NRI socket and Anchor state host paths to every other namespace and
service account. A second validation denies the runtime bootstrap-bypass
annotation to every pod except the Anchor node DaemonSet's namespace and
service account. Pod Security Admission alone is not treated as sufficient
authorization for either privilege.

### IAM surface

The node component remains credential-free. In addition to the existing
inventory and `ip-reassign` permissions, a controller that enables
`route-repoint` requires:

- `ec2:DescribeRouteTables` and the existing ENI/subnet discovery reads;
- `ec2:CreateRoute` and `ec2:ReplaceRoute` on administrator-designated managed
  route tables; and
- read access needed to verify ENI source/destination-check and the resulting
  route targets.

Route mutations are restricted to route-table ARNs carrying the configured
Anchor management tag. EC2 describe actions that do not support equivalent
resource scoping remain read-only. Anchor validates source/destination-check
as an installation prerequisite in `0.2`; it does not broaden the runtime
controller policy to mutate that setting unless a later ADR requires it.

Account route quota discovery is an installer or preflight responsibility.
If it queries AWS Service Quotas, `servicequotas:GetServiceQuota` belongs to
that preflight identity and is not automatically added to the long-running
controller. The exact IAM policy and unsupported tag/condition combinations
must be proven in the AWS qualification rather than inferred from the API
names.

For `0.2`, the route-repoint qualification and support target is Transit Gateway.
Direct Connect and Virtual Private Gateway topologies remain unsupported until
their aggregate advertisement, ingress lookup, and return-path behavior are
qualified separately; they do not block the `0.2` release.

### Fail-closed runtime contract

Supported nodes must configure the runtime to require the Anchor NRI plugin.
A prepared workload must not start merely because the plugin is absent or
restarting.

For containerd, the primary design uses the NRI default validator's
[`required_plugins`](https://github.com/containerd/containerd/blob/main/docs/NRI.md)
setting and its `tolerate_missing_plugins_annotation` bootstrap mechanism. The
configured annotation is present only on the Anchor node DaemonSet, allowing
the plugin container to start before Anchor registers while ordinary workloads
remain subject to the required-plugin check. Admission rejects that annotation
on every other pod, including tenant-controlled workloads. The final annotation
key uses Anchor's release API domain; it is not exposed as a service-layer
workload contract.

This design is not accepted by configuration inspection alone. Qualification
must prove, for every supported containerd version, the complete sequence when
Anchor is absent, starts late, restarts, or reconnects: sandbox handling,
blocking of init and application containers, NRI `Synchronize`, interface
injection, and eventual container start. It must also prove that the primary
CNI and `eth0` are ready before the chosen injection event and that partial
injection never permits container start.

Node bootstrap additionally installs an API-domain-qualified
`<anchor-api-domain>/not-ready:NoSchedule` taint. Anchor removes it only after
registering, synchronizing, and passing its local health checks. Only the
Anchor DaemonSet tolerates the taint. This prevents new placements on an
obviously unhealthy node, but it is defense in depth, not the runtime
fail-closed mechanism: a scheduling taint cannot protect already bound pods or
cover every plugin failure after scheduling.

If the validator's bootstrap annotation cannot provide the proven behavior,
the fallback is a host-installed, runtime-started Anchor NRI shim that is
registered before Kubernetes workloads are admitted. A taint by itself is not
an acceptable fallback. Platforms for which neither design can prove
fail-closed behavior are unsupported.

Runtime, NRI, primary-CNI, and Kubernetes versions are part of the Anchor
support matrix. The required-plugin/bootstrap behavior and hook ordering are
qualified independently for containerd and any future CRI-O support. CRI-O is
unsupported until that separate qualification is complete.

For the 0.2 release, the self-managed AWS platform pins containerd 2.2.6 and
requires the NRI sandbox rollback fix from containerd PR 13399 for the
`RunPodSandbox` path. A platform's nominal containerd version is not sufficient
evidence when distributors carry their own backports. Containerd 2.2.5 uses
the `CreateContainer` compatibility path. Stock EKS AL2023 images containing
2.2.5 are therefore a supported target without a custom AMI, but their exact
package and EKS integration must pass separate fail-closed, ordering, and
recovery qualification before production rollout. `nodeadm` merges the
validator configuration but does not change Anchor's runtime selection or
replace platform qualification.

### Level-triggered recovery

NRI hooks are triggers, not the source of truth. The node component persists a
plan before DRA `NodePrepareResources` succeeds, including:

- pod, claim, request, and allocation identities;
- expected parent identity and host name per path;
- child interface name, addresses, routes, and rules;
- placement generation and observed cloud readiness; and
- local injection phase and last observation.

The local attachment state machine is:

```text
Prepared -> InjectionPending -> Injected
                 |              |
                 +-> Recovering <-+
                         |
                         +-> Injected after repair

Any non-retained phase -> Unpreparing -> Retained
```

Recovery is driven by:

- NRI `Synchronize` after registration or restart;
- current pods, claims, allocations, and placement objects;
- persistent local state;
- periodic inspection of host parents and live pod namespaces; and
- controller-side cloud ownership and drift verification.

Missing, stale, or inconsistent plans fail closed. Reconciliation is
idempotent. The periodic node loop verifies both host-parent identity and the
interfaces, addresses, routes, and rules in every recorded live pod namespace.
It repairs drift by rerunning the same idempotent injection operation. A
workload-specific corrupt or unrecoverable plan is persisted as `Recovering`
and reported through status, events, and metrics without marking the entire
plugin unhealthy or withdrawing unrelated devices; plan-store access and NRI
connection failures remain plugin-level faults. Kernel cleanup of ipvlan
children is a safety property, not the cleanup protocol.

### Unprepare, release, and endpoint retirement

DRA unprepare and claim release are deliberately non-destructive to carrier
placement. They remove or reconcile local pod interfaces, but they do not make
the externally reachable carrier identity ownerless:

| Strategy | State retained after unprepare or release |
|---|---|
| `ip-reassign` | The address remains assigned to the last successfully placed ENI. |
| `route-repoint` | The managed routes continue to target the last successfully placed ENI. |
| `l2-announce` | The durable logical owner is retained; replacement from a NotReady node remains blocked until the provider records explicit fencing evidence. |

The next successful placement moves or repoints the identity and advances its
generation. Automatic garbage collection never deletes `EndpointOwnership`
records or treats claim deletion as authorization to remove cloud routes or
addresses. The node does garbage-collect a retained local plan file after its
claim UID no longer exists; that bounded local cleanup does not affect cloud
placement or durable ownership. `Retained` is therefore the normal end of a
local pod attachment, not deletion of endpoint ownership.

Permanent endpoint retirement is a separate, explicit administrator workflow
with validation, audit status, and strategy-specific cleanup. Only that
workflow may transition the durable endpoint lifecycle to `Retired`; no
timeout, pod deletion, unprepare call, or missing claim performs retirement.

### On-premises adoption

On-premises Anchor adoption is an explicit, separately planned workstream. It
includes Kubernetes/DRA version alignment, NRI enablement, VLAN inventory,
ownership fencing, L2 announcement, and conformance testing.

It does not block implementing the common architecture on AWS, but the common
interfaces and service-layer ABI must not be specialized in ways that prevent
the on-premises adapters. Fleet-wide Multus retirement is complete only when
both environments use the common lifecycle.

## Consequences

### Positive

- One ResourceClaim-based workload contract replaces the claim-plus-NAD
  contract.
- AWS and on-premises converge on the same interface ABI and driver lifecycle.
- Generated NADs, pod network annotations, and claim-to-NAD naming conventions
  are removed.
- Multus and three secondary CNI binaries leave Anchor's target qualification
  matrix.
- Direct netlink configuration can represent routed `/32`s and explicit
  per-path tables without CNI-chain encoding constraints.
- ipvlan retains endpoint density and low-risk child cleanup.
- Cloud-specific behavior remains behind narrow inventory and placement seams.

### Negative

- Anchor owns netlink configuration, idempotency, rollback, and recovery.
- NRI is a mandatory runtime integration and expands the supported-platform
  contract.
- Fail-closed NRI configuration requires node-image or runtime control.
- NRI API changes before stability may require coordinated Anchor updates.
- Route-repoint correctness depends on AWS topology and route-table coverage,
  not only on Anchor code.
- A placement followed by persistent local injection failure remains a
  blackhole until reconciliation succeeds or an operator initiates recovery.
- On-premises convergence is a distinct infrastructure program.

## Alternatives considered

### Keep Multus in `0.2` and migrate later

Rejected. There are no production users requiring compatibility, and an
interim implementation would be built, qualified, documented, and then
removed. It would also risk making NAD names and annotations an accidental
public API.

### Support Multus and NRI as parallel injectors

Rejected. Parallel injectors create two lifecycle, recovery, documentation,
and conformance matrices indefinitely.

### Move dedicated physical ENIs into AWS pods

Rejected as the initial default. It reduces endpoint density, makes correct
device return critical, and diverges from the established on-premises ipvlan
dataplane. The internal injector seam preserves it as a future option when a
demonstrated requirement such as per-endpoint security groups, a real MAC, EFA,
or hardware isolation justifies the cost.

### Gate AWS development on on-premises adoption

Rejected. On-premises adoption is valuable for parity and ownership safety but
is a separate program. AWS implementation can proceed against the common ABI
and extension seams without waiting for that rollout.

## Release acceptance criteria

Anchor `0.2` is not released until:

- the pod manifest contains only the ResourceClaim attachment contract;
- no Multus, NAD, `ipvlan` CNI, `static` CNI, or `sbr` CNI dependency remains;
- `ResourceSlice` publication implements the complete path-bundle device and
  capacity model;
- the runtime fails closed when the Anchor NRI plugin is absent, including
  during bootstrap, restart, reconnect, and sandbox recreation;
- the tested NRI hook ordering proves that the primary CNI is ready before
  injection and no init or application container starts before full injection;
- node-plugin, runtime, and pod restarts converge from persistent state;
- dual-path interface names, addresses, routes, and rules are verified in the
  pod namespace;
- partial injection rolls back or converges without starting the workload;
- persistent injection failure produces the documented condition, events,
  metrics, alerts, and operator recovery path without withdrawing unrelated
  capacity; plugin-level failure withdraws the node's devices;
- unprepare and claim release retain placement and ownership for every enabled
  strategy, while explicit endpoint retirement is independently tested;
- the AWS `ip-reassign` e2e passes through the NRI injector;
- the route-repoint dataplane qualification records route-table coverage,
  cutover timing, packet loss, and SCTP behavior before that strategy is marked
  supported;
- route-repoint IAM, resource-tag restrictions, source/destination-check
  validation, and account route-capacity preflight are tested;
- admission tests prove that only the Anchor service account can mount the NRI
  socket, live network namespace directory, and persistent state paths or use
  the required-plugin bootstrap annotation;
- the documented platform matrix matches the tested container runtime,
  primary CNI, Kubernetes, and NRI versions; and
- `0.1` artifacts remain immutable and all adopting infrastructure and service
  releases explicitly pin the `0.2` line.

## Follow-up work

1. Run the AWS shared-parent ipvlan route-repoint dataplane, route-table
   coverage, and cutover qualification before advertising the strategy or
   marking it supported. Record a new ADR if its result invalidates the
   shared-parent architecture.
2. Qualify containerd required-plugin bootstrap and NRI/primary-CNI hook
   ordering for both version-selected lifecycle events, including late
   registration, restart, reconnect, synchronization, sandbox recreation, and
   partial injection.
3. Specify and test the DRA path-bundle device model, non-destructive release,
   explicit retirement, injection-failure signals, and operator recovery.
4. Re-qualify AWS `ip-reassign` without Multus in `aws-staging`.
5. Complete privileged Linux integration tests for the netlink injector in
   addition to its unit-tested idempotency, verification, and rollback core.
6. Qualify the implemented `RunPodSandbox` and `CreateContainer` hooks,
   `Synchronize`, fail-closed behavior, periodic repair, and retained-plan
   garbage collection under runtime and node restarts.
7. Validate the coordinated Helm, Terraform, Kubernetes-bootstrap, and service
   release in `aws-staging`, including taint ownership, admission policy, host
   paths, and IAM.
8. Implement and advertise `route-repoint` only against the qualified network
   plan.
9. Remove any remaining Multus-era artifacts before the first `0.2` release
   candidate.
10. Keep published `0.1` artifacts immutable while cutting coordinated `0.2`
    release candidates across the repositories.
11. Extend on-premises qualification from the local kubeadm/VDE harness to
    supported VMware/OpenStack switching, anti-spoofing, and provider fencing.
