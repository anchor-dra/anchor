# Architecture

Anchor adds static secondary pod endpoints without replacing the primary CNI.
The Kubernetes scheduler allocates one logical DRA endpoint slot. The node
plugin blocks pod preparation while the central controller moves every claimed
private IP to the target node's pre-attached ENIs. Multus then creates ipvlan
L2 children inside the pod network namespace.

```text
ResourceClaim -> scheduler -> ResourceSlice endpoint slot -> target node
                                                        |
kubelet -> anchor node -> EndpointPlacement -> anchor controller -> EC2
                                                        |
                                                        +-> EndpointOwnership
                                                        +-> claim-specific NADs
                                                        +-> AssignPrivateIpAddresses
```

The central controller is the only component with AWS credentials. It validates
ENI tags and subnets, writes `AnchorNodeInventory`, and applies one shared rate
limit to all EC2 discovery and mutation calls. The node plugin maps validated
ENI MACs to host interface names, brings those links up, and publishes
ResourceSlices only for complete profiles.

`EndpointPlacement` is an internal prepare request, not an AWS authorization
document. Before any mutation, the controller reconstructs the expected
placement from the ResourceClaim allocation, the exact reserved Pod UID and
node, and the controller-written inventory spec. It accepts only the host
interface mapping from node-written inventory status, and only when all AWS
fields still match that spec. The claim annotation is the sole source of
`force-steal`. A mismatched request is marked Failed without calling AWS. This
keeps a compromised node plugin from selecting an address, ENI, or another
node for the credentialed controller.

One DRA device represents the complete set of paths in a profile. This ensures
all addresses for a multihomed endpoint schedule together. The alpha defaults
to one endpoint slot per node; administrators may raise the value within the
secondary-address capacity of every carrier ENI.

## Reconciliation scaling

Pending placements are checked every two seconds, while inventory and drift
checks run every 60 seconds. Inventory refresh batches all enabled instance
IDs and carrier subnet IDs. Placement batches target-ENI and current-owner
lookups across all pending paths; Ready endpoints use one batched, read-only
owner lookup and enter the placement path only when drift is detected. AWS
pagination and a shared rate limiter apply to every batch.

## Pod routing

Each DeviceClass path may define one explicit IPv4 gateway and a list of
remote IPv4 CIDRs. The node plugin carries that admin-owned configuration into
the EndpointPlacement, and the controller writes it into the claim-specific
static-IPAM NAD. The chained `sbr` plugin then gives traffic bound to each
claimed address its own routing table and carrier-interface gateway.

When gateway and routes are omitted, the path intentionally supports only its
directly connected subnet. Anchor never derives a gateway from a subnet CIDR.
These pod routes are independent from the future AWS `route-repoint` placement
strategy and from infrastructure-owned DX/TGW route advertisement.

## Failure semantics

- A path failure is handled by SCTP without an EC2 mutation.
- A pod move calls `AssignPrivateIpAddresses` once per path with
  `AllowReassignment=true`.
- `EndpointPlacement` is execution state for one ResourceClaim UID and is
  garbage-collected with that claim. Its NADs are garbage-collected with the
  placement.
- Cluster-scoped `EndpointOwnership` records retain the logical
  namespace/name owner and last ENI for each IP. They survive claim and
  namespace recreation.
- A recreated claim with the same namespace/name reuses the recorded ENI as
  trusted previous ownership and does not require `force-steal`.
- A prepare succeeds only after every path is placed and its NAD is ready.
- Successful paths are retained across partial retries.
- Unprepare is deliberately non-destructive; the next placement moves the IP.
- Unknown address owners are never overwritten without the explicit
  `dra.anchordra.co/force-steal: "true"` claim annotation.
- A different logical claim cannot take an established address unless
  `force-steal` is explicit and the AWS reassignment succeeds. Failed mutations
  never transfer the durable ownership record.

## Ownership scope and multi-cluster limitation

`EndpointOwnership` is durable across claim and namespace recreation, but it is
stored only in the Kubernetes cluster. Anchor 0.1 has no AWS-side ownership
record and does not coordinate between clusters. Operate only one active Anchor
installation for any given static address. Clusters may share a subnet only
when their claimed address sets cannot overlap.

A second cluster without `force-steal` refuses to move an address from an
unknown ENI. If two active clusters both retain `force-steal`, they can
repeatedly reassign the address between their ENIs. Ready placements detect
this through the drift check, which defaults to 60 seconds; failed placements
retry on the two-second pending reconciliation interval.

For disaster recovery, restore the Kubernetes ownership records when possible.
If they are unavailable, fence the old controller by stopping it or removing
its AWS mutation permission before authorizing a force takeover. `force-steal`
is a temporary recovery action and must be removed after the new placement is
Ready.

AWS tags apply to an ENI, not to an individual secondary private IPv4 address,
and tagging is not atomic with IP reassignment. A later release may require an
immutable installation-identity tag on carrier ENIs to reject foreign-cluster
ownership. True active/standby multi-cluster coordination requires an external,
strongly consistent lease and is outside the 0.1 scope.

The alpha implements IPv4 and `ip-reassign` within one AZ. AWS route repointing,
pool allocation, IPv6, cross-AZ mobility, and ENI lifecycle management are not
implemented.
