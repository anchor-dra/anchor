# Optional remote-subnet preferences

This feature requires Anchor 0.2.2 or newer. It is **not available in the published 0.2.1 image**. Do not enable the
field until a supporting controller/node build is installed.

## Contract

An administrator may add `preferredDestinations` to a DeviceClass path:

```yaml
# Fragment of an existing, otherwise complete path configuration
name: a
interfaceName: net1
routingTable: 101
routes: [10.201.0.0/24, 10.201.1.0/24]
preferredDestinations: [10.201.0.0/24]
```

Path B can retain the same reachable `routes` while preferring `10.201.1.0/24`.
The field is optional, defaults to empty, and belongs to the platform's
DeviceClass configuration. Tenant ResourceClaims and Pod claim references are
unchanged. Application deployments do not need AWS subnet or route-table IDs.

Each preference must be a canonical IPv4 CIDR covered by one of that path's
explicit `routes`. Duplicate and overlapping preferences are rejected, both
within and between paths. There is no inferred A/B ordering of remote CIDRs.

## Routing behavior

Anchor installs policy rules in this order:

| Priority | Selector | Lookup |
|---|---|---|
| `10000 + routingTable` | claimed source address | existing path table |
| `20000 + routingTable` | each preferred destination prefix | same path table |
| `32766` | normal kernel fallback | main table |

Thus an explicitly selected carrier source retains its existing path, including
cross-path destinations. An initial lookup with no carrier source can use the
destination preference to select an interface and a source address bound by SCTP.
These are IP routing rules; they apply to matching traffic regardless of protocol.

No preferences means no new destination rules. An address with a connected prefix,
such as `10.50.1.10/24`, still supplies the usual main-table route to same-subnet
peers. A routed `/32` endpoint does not acquire that connected route merely because
its address belongs to a wider logical endpoint pool.

The per-path route lists remain reachability configuration. Preferences do not
remove routes, isolate A/B networks, or guarantee independent physical paths.

## Lifecycle

Preferences are copied into the approved placement and persisted node plan.
Verification checks every expected rule; repeated configuration does not add
duplicates. Missing rules trigger the existing drift-repair flow. Equal-priority
overlapping rules with conflicting lookups are rejected rather than silently
accepted. Cleanup removes matching source and destination rules even if the
interface has already disappeared, including injection rollback. Rules with
different selectors, such as firewall marks, are not adopted or removed.

Deleting a Pod's network namespace removes its kernel rules. Existing
ResourceClaims retain their allocated DeviceClass configuration; changing a
DeviceClass does not rewrite those allocations. Roll out the supporting Anchor
release first, then update the class and use the established controlled Pod/claim
recreation workflow. Preserve logical endpoint identity and review ownership
before recreating claims. A Pod restart with the same old allocation is not a
configuration migration.

## Verification

Model tests cover optional/default behavior, propagation, CIDR validation,
coverage and overlap rejection. Controller tests cover configuration comparison.
The Linux integration test uses real network namespaces and ipvlan interfaces to
check source selection, explicit cross-path lookups, idempotency, missing-rule
repair, conflicting-rule detection, cleanup and same-subnet routing without
preferences:

```sh
# Run only in an isolated privileged Linux test environment.
ANCHOR_NETNS_TEST=1 go test ./internal/node -run TestPolicyRulesKernelLifecycle -v
```

The earlier Ireland/London SCTP experiment validated this rule ordering using
temporary rules. The new packaged implementation still needs release and EKS
qualification; a passing kernel test does not replace that deployment gate.
