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
limit to all EC2 discovery and mutation calls. The node plugin maps validated ENI MACs to host interface names,
brings those links up, and publishes ResourceSlices only for complete profiles.

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

The alpha implements IPv4 and `ip-reassign` within one AZ. Route repointing,
pool allocation, IPv6, cross-AZ mobility, and ENI lifecycle management are not
implemented.
