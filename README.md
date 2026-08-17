# Anchor

Anchor is an experimental Kubernetes Dynamic Resource Allocation driver for
moving static AWS private IPv4 endpoints between same-AZ EC2 nodes. It keeps
the primary CNI untouched and uses Multus with ipvlan L2 for secondary pod
interfaces.

The current `0.1.x` scope is the `ip-reassign` strategy only. It requires
Kubernetes 1.34 or newer, Multus, the standard `ipvlan`, `static`, and `sbr`
CNI plugins, and pre-attached tagged ENIs.

The temporary module and driver names use the reserved `example.com` domain.
They must be replaced before publishing the project.

See [the architecture](docs/architecture.md), [installation guide](docs/install.md),
and [AWS staging test](docs/e2e-aws.md).
