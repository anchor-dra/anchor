# AWS staging end-to-end test

This is the maintainer qualification for the `ip-reassign` strategy. It creates
disposable resources from `hack/e2e/aws`; it does not modify the platform
Terraform state. Run it from a connected workstation with AWS SSO, the cluster
API tunnel, `kubectl`, Helm, Terraform, Docker, Ansible, and `jq` available.

The topology requalified on 2026-08-18 was:

- region/AZ: `eu-west-1` / `eu-west-1a`
- two endpoint workers, each with one pre-attached ENI per carrier path
- two carrier subnets and one dual-homed SCTP peer in the same AZ
- bidirectional security-group and network-ACL rules for SCTP between paths

The 0.2 qualification target is Kubernetes 1.35.4 with containerd 2.2.6, NRI,
and Calico. On 2.2.6, `RunPodSandbox` is primary and `CreateContainer` is the
late-registration guard. The 2.2.5 compatibility qualification uses only
`CreateContainer` and
must prove that failed injection leaves one reusable sandbox and starts no
containers. Kubernetes or runtime minor upgrades are breaking platform changes
and must be validated in the infrastructure layer before Anchor qualification.
This proves the 2.2.5 runtime workaround for self-managed nodes. Stock EKS
AL2023 with 2.2.5 is also a supported target, but its `nodeadm` bootstrap,
required-plugin enforcement, CNI ordering, and cold restart behavior require a
separate run on the selected EKS AMI release.

## 1. Provision the disposable substrate

Copy `terraform.tfvars.example` to ignored `terraform.tfvars`, fill the bastion
security group and the real IAM role ARN behind the SSO session, then run:

```bash
export AWS_PROFILE=anchor-e2e
export KUBE_CONTEXT=anchor-e2e
terraform -chdir=hack/e2e/aws init
terraform -chdir=hack/e2e/aws plan -out=.work/e2e.tfplan
terraform -chdir=hack/e2e/aws apply .work/e2e.tfplan
```

The module creates one temporary worker, a two-interface SCTP peer, two carrier
ENIs on both endpoint workers, the controller IAM role, and the test security
group. Its state and plan files are ignored but must be kept until cleanup.

## 2. Join the temporary worker

Render a temporary Kubespray inventory from the existing inventory:

```bash
worker=$(terraform -chdir=hack/e2e/aws output -json worker)
worker_dns=$(jq -r .private_dns <<<"$worker")
worker_ip=$(jq -r .private_ip <<<"$worker")
hack/e2e/render-inventory.sh \
  /path/to/kubespray/inventory/anchor/hosts.ini \
  .work/hosts.ini "$worker_dns" "$worker_ip"
```

From the pinned Kubespray checkout, add only the new node:

```bash
ansible-playbook -i .work/hosts.ini \
  -e @/path/to/kubespray/inventory/anchor/group_vars/k8s_cluster/k8s-cluster.yml \
  -e kube_version=1.35.4 \
  -e nri_enabled=true \
  -b scale.yml \
  --limit="$worker_dns"
```

Verify Kubernetes exposes `resource.k8s.io/v1`, both workers are Ready, and
containerd has the NRI default validator configured with `anchor` as a required
plugin. Before installing Anchor, prove an ordinary test pod fails closed.

## 3. Install the Anchor RC

Wait for the `develop` workflow to publish the RC and make its GHCR packages
public. Pull the chart once from the connected workstation to prove it is
available without credentials. Only the SCTP test workload is built locally and
imported into containerd on both endpoint workers:

```bash
ANCHOR_VERSION=0.2.0-rc.1
helm pull oci://ghcr.io/anchor-dra/charts/anchor \
  --version "$ANCHOR_VERSION"

docker build --platform linux/amd64 \
  -f hack/e2e/Dockerfile.sctp -t anchor-sctp-e2e:0.2.0 .
docker save -o .work/anchor-sctp-e2e.tar anchor-sctp-e2e:0.2.0

ansible kube_node -i .work/hosts.ini --limit=ENDPOINT_NODES -b \
  -m copy -a 'src=.work/anchor-sctp-e2e.tar dest=/tmp/anchor-sctp-e2e.tar mode=0600'
ansible kube_node -i .work/hosts.ini --limit=ENDPOINT_NODES -b \
  -m shell -a 'ctr -n k8s.io images import /tmp/anchor-sctp-e2e.tar >/dev/null'
```

Label only the two workers with the test carrier ENIs. The staging
control-plane instance profile must already carry the tag-scoped Anchor policy
described in the IAM guide. Install the published chart with the controller on
those control-plane nodes, then apply the examples:

```bash
kubectl --context "$KUBE_CONTEXT" label node ENDPOINT_NODE_1 ENDPOINT_NODE_2 \
  dra.anchordra.co/enabled=true
kubectl --context "$KUBE_CONTEXT" taint node ENDPOINT_NODE_1 ENDPOINT_NODE_2 \
  dra.anchordra.co/not-ready=true:NoSchedule
kubectl --context "$KUBE_CONTEXT" create namespace anchor-system
kubectl --context "$KUBE_CONTEXT" label namespace anchor-system \
  pod-security.kubernetes.io/enforce=privileged --overwrite

helm --kube-context "$KUBE_CONTEXT" upgrade --install anchor \
  oci://ghcr.io/anchor-dra/charts/anchor \
  --version "$ANCHOR_VERSION" \
  --namespace anchor-system \
  --set aws.region=eu-west-1 \
  --set aws.useInstanceProfile=true \
  --set 'controller.nodeSelector.node-role\.kubernetes\.io/control-plane=' \
  --set 'controller.tolerations[0].key=node-role.kubernetes.io/control-plane' \
  --set 'controller.tolerations[0].operator=Exists' \
  --set 'controller.tolerations[0].effect=NoSchedule'

kubectl --context "$KUBE_CONTEXT" apply -f examples/aws-ip-reassign/deviceclass.yaml
kubectl --context "$KUBE_CONTEXT" apply -f examples/aws-ip-reassign/claim.yaml
kubectl --context "$KUBE_CONTEXT" apply -f examples/aws-ip-reassign/pod.yaml
kubectl --context "$KUBE_CONTEXT" -n anchor-e2e wait \
  --for=condition=Ready pod/smsc-sctp --timeout=180s
```

## 4. Verify placement and SCTP

Verify that two ResourceSlices exist, the claim is allocated and reserved, the
EndpointPlacement is Ready, and the pod has both addresses:

```bash
kubectl --context "$KUBE_CONTEXT" get resourceslices.resource.k8s.io
kubectl --context "$KUBE_CONTEXT" -n anchor-e2e get resourceclaim,endpointplacement
kubectl --context "$KUBE_CONTEXT" -n anchor-e2e exec smsc-sctp -- ip -d address
kubectl --context "$KUBE_CONTEXT" -n anchor-e2e exec smsc-sctp -- ip rule
```

Install `lksctp-tools` from the peer's configured OS repository and start a
multihomed listener:

```bash
sctp_test -H PEER_PATH_A_IP -B PEER_PATH_B_IP -P 2905 -l
```

The example deliberately crosses the existing peer paths to prove off-subnet
routing without adding more AWS resources. Verify that traffic sourced from
path A reaches the path-B peer through `sigtran-a`, and vice versa:

```bash
kubectl --context "$KUBE_CONTEXT" -n anchor-e2e exec smsc-sctp -- \
  ip route get 10.40.36.20 from 10.40.32.10
kubectl --context "$KUBE_CONTEXT" -n anchor-e2e exec smsc-sctp -- \
  ip route get 10.40.32.20 from 10.40.36.10
```

The first result must use `sigtran-a` through `10.40.32.1`; the second must use
`sigtran-b` through `10.40.36.1`. Prove both crossed paths separately:

```bash
kubectl --context "$KUBE_CONTEXT" -n anchor-e2e exec smsc-sctp -- \
  sctp_test -H 10.40.32.10 -P 2906 -C PEER_PATH_B_IP -p 2905 -s -x 1 -c 0
kubectl --context "$KUBE_CONTEXT" -n anchor-e2e exec smsc-sctp -- \
  sctp_test -H 10.40.36.10 -P 2906 -C PEER_PATH_A_IP -p 2905 -s -x 1 -c 0
```

Finally, send from both claimed addresses in the pod:

```bash
sctp_test -H 10.40.32.10 -B 10.40.36.10 -P 2906 \
  -C PEER_PATH_A_IP -C PEER_PATH_B_IP -p 2905 -s -x 1 -c 0
```

The peer must report `SCTP_ASSOC_CHANGE(COMMUNICATION_UP)` and receive data.
For a path-failure test, keep the association open, drop SCTP traffic to one
peer address, and inspect `/proc/net/sctp/remaddr`: the failed path changes to
inactive while the other remains active and the association stays present.
Always remove the injected firewall rule immediately afterward.

## 5. Verify node rescheduling

Cordon the current endpoint worker, delete and recreate only the test pod, and
keep the standalone claim:

```bash
kubectl --context "$KUBE_CONTEXT" cordon CURRENT_ENDPOINT_NODE
kubectl --context "$KUBE_CONTEXT" -n anchor-e2e delete pod/smsc-sctp --wait
kubectl --context "$KUBE_CONTEXT" apply -f examples/aws-ip-reassign/pod.yaml
kubectl --context "$KUBE_CONTEXT" -n anchor-e2e wait \
  --for=condition=Ready pod/smsc-sctp --timeout=180s
kubectl --context "$KUBE_CONTEXT" uncordon CURRENT_ENDPOINT_NODE
```

The replacement must run on the other endpoint worker. Verify that both AWS
private addresses moved to that worker's tagged ENIs, the claim has no
`force-steal` annotation, and the same SCTP command establishes a new
association. On 2026-08-17, pod recreation through Ready took 7.73 seconds
with the default two-request-per-second global EC2 budget, excluding Kubernetes
node-failure detection.

## 6. Verify claim and namespace recreation

Capture the claim UID, delete the pod and claim, and recreate both while the
current endpoint node is cordoned:

```bash
old_claim_uid=$(kubectl --context "$KUBE_CONTEXT" -n anchor-e2e \
  get resourceclaim smsc-sigtran-endpoint -o jsonpath='{.metadata.uid}')
current_node=$(kubectl --context "$KUBE_CONTEXT" -n anchor-e2e \
  get pod smsc-sctp -o jsonpath='{.spec.nodeName}')
kubectl --context "$KUBE_CONTEXT" cordon "$current_node"
kubectl --context "$KUBE_CONTEXT" -n anchor-e2e delete pod smsc-sctp --wait
kubectl --context "$KUBE_CONTEXT" -n anchor-e2e delete resourceclaim \
  smsc-sigtran-endpoint --wait
kubectl --context "$KUBE_CONTEXT" apply -f examples/aws-ip-reassign/claim.yaml
kubectl --context "$KUBE_CONTEXT" apply -f examples/aws-ip-reassign/pod.yaml
kubectl --context "$KUBE_CONTEXT" -n anchor-e2e wait \
  --for=condition=Ready pod/smsc-sctp --timeout=180s
new_claim_uid=$(kubectl --context "$KUBE_CONTEXT" -n anchor-e2e \
  get resourceclaim smsc-sigtran-endpoint -o jsonpath='{.metadata.uid}')
test "$old_claim_uid" != "$new_claim_uid"
kubectl --context "$KUBE_CONTEXT" uncordon "$current_node"
```

The old UID-named EndpointPlacement must be gone. The replacement
must become Ready on the other worker without `force-steal`, while the two
cluster-scoped ownership records keep the same logical owner and update to the
new claim UID and ENIs:

```bash
kubectl --context "$KUBE_CONTEXT" get endpointownerships.dra.anchordra.co -o wide
kubectl --context "$KUBE_CONTEXT" -n anchor-e2e get endpointplacements
```

Repeat once with `kubectl delete namespace anchor-e2e --wait` instead of
deleting only the claim. Reapply `claim.yaml` and `pod.yaml`, and verify the new
namespace and claim incarnation reclaim the same two addresses without force.
The EndpointOwnership objects must remain present throughout namespace
termination.

## Evidence and cleanup

Record the following evidence under ignored `.work/evidence/`:

- node and system pod health before and after the Kubernetes upgrade
- ResourceClaim allocation and ResourceSlices
- EndpointPlacement, EndpointOwnership, owner-reference, and Kubernetes event
  state before and after claim and namespace recreation
- containerd required-plugin failure tests for absence, late start, restart,
  reconnect, and sandbox recreation
- the selected NRI hook from registration logs and, on 2.2.5, stable sandbox
  identity and count across repeated failed `CreateContainer` attempts
- persisted plan phase and injection failure/retry metrics
- `ip -d address`, policy routes, and both crossed off-subnet route lookups
  inside the pod
- EC2 private-IP ownership before and after rescheduling
- SCTP association and remote-path state proving both pod addresses are bound
  without NAT
- placement call count and latency

Before destroying Terraform, remove the temporary node with Kubespray
`remove-node.yml`. Destruction detaches and deletes only ENIs, instances,
security groups, and IAM resources held in the Anchor e2e state.

Never destroy the Terraform state before removing the temporary Kubernetes
node, and never delete carrier IP assignments as part of a Helm uninstall.
