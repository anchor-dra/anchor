# Architecture

Anchor 0.2 adds static secondary pod endpoints through one DRA contract. The
service workload supplies only a `ResourceClaim`; it does not name a Multus
attachment or carry CNI configuration.

```text
ResourceClaim -> scheduler -> path-bundle ResourceSlice -> selected node
                                                        |
kubelet DRA prepare -> persistent PodNetworkPlan -> EndpointPlacement
                                                |                |
                                                |       controller -> AWS or L2 ownership fence
                                                |                |
primary CNI -> eth0 -> Anchor NRI -> ipvlan/netlink      EndpointOwnership
```

One advertised DRA device is one complete profile slot on one node. It includes
every required carrier path so a dual-path endpoint cannot be split across
nodes. A node publishes slots only while every profile parent is present and
the local NRI plugin is registered, synchronized, and healthy.

On AWS, the central controller is the only component with AWS credentials. It
discovers tagged ENIs and performs `ip-reassign`. On-premises, the
administrator-owned `DeviceClass` declares each host parent and subnet; the
controller writes that approved identity into node inventory and uses
`EndpointOwnership` to fence logical address ownership. The node verifies the
declared link and reports its MAC before advertising capacity.

## DRA and NRI lifecycle

Before DRA prepare succeeds, the node stores a versioned `PodNetworkPlan` under
`/var/lib/anchor/plans`. It records pod, claim, allocation, parent, placement,
address, interface, route, and rule identities. Cloud placement must become
Ready before the plan enters `InjectionPending`.

Anchor selects its injection event from the containerd version reported by the
NRI registration handshake. Containerd 2.2.5 and older use `CreateContainer`
so an injection error cannot enter the affected sandbox rollback path;
containerd 2.2.6 and newer use `RunPodSandbox` as the primary hook and retain
`CreateContainer` as an idempotent late-registration guard. An unknown version
takes the conservative `CreateContainer` path. At either event Anchor resolves plans by
the exact Kubernetes pod UID, including template-generated claims through
`Pod.status.resourceClaimStatuses`, checks the sandbox identity and network
namespace, creates one ipvlan L2 child per path, and configures it directly
through netlink. Each profile path owns:

- a stable `interfaceName`, such as `sigtran-a`;
- one IPv4 address;
- an explicit Linux `routingTable`;
- the connected and link-scope gateway routes; and
- source-specific rules and administrator-defined peer routes.

Injection is idempotent and verified before the callback returns. A partial
failure removes children created during that attempt, returns an NRI error so
containers cannot start, persists `Recovering`, emits
`NetworkInjectionFailed`, and updates retry/failure-age metrics. `Synchronize`
and a 30-second level-triggered loop inspect host parents and live pod network
namespaces. Drift is repaired through the same idempotent injection path. A bad
workload plan is isolated from plugin health and unrelated DRA capacity.

## Fail-closed nodes

Containerd's NRI default validator must require the `anchor` plugin. Only the
Anchor DaemonSet carries
`dra.anchordra.co/tolerate-missing-nri-plugin: "true"`, which avoids the plugin
bootstrap deadlock. Candidate nodes also carry the
`dra.anchordra.co/not-ready:NoSchedule` taint. Anchor removes it only after NRI
synchronization and local parent checks and restores it when health is lost.
The leader-elected controller owns taint updates from node-written inventory
status; the privileged node agent has read-only Node RBAC.

The chart mounts the NRI socket and persistent state only into the privileged
node DaemonSet. A `ValidatingAdmissionPolicy` rejects those host paths and the
NRI bootstrap-bypass annotation for all other namespaces and service accounts.
The node has no AWS credentials.

## Ownership and failure semantics

`EndpointPlacement` is execution state for one claim UID.
`EndpointOwnership` is durable endpoint identity and intentionally survives pod,
claim, namespace, and Helm release deletion. DRA unprepare changes the local
plan to `Retained`; it never unassigns the AWS address. Permanent retirement is
a separate administrator action. Retained local plan files are removed once
their exact claim UID no longer exists; durable `EndpointOwnership` is not.

An unknown address owner is never overwritten unless the claim explicitly
sets `dra.anchordra.co/force-steal: "true"`. Anchor supports one active
installation per address set. It restores reachability after reconciliation;
it does not promise to preserve a live SCTP association through pod or node
failure.

The enabled 0.2 strategies are same-AZ AWS IPv4 `ip-reassign` and on-premises
`l2-announce`. The latter sends gratuitous ARP only after the ipvlan endpoint is
fully configured. A move from a healthy node is serialized by DRA unprepare; a
move from a NotReady node requires the provider or operator to record
`dra.anchordra.co/fenced=true` after power or hypervisor fencing. This prevents
the Kubernetes control-plane view from being treated as proof that a stale
on-prem workload stopped.

`route-repoint` remains a configuration seam but is rejected by the controller
and omitted from published DRA inventory until the shared-parent `/32`
dataplane and IAM design pass staging qualification.
