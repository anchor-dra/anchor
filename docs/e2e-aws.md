# AWS staging end-to-end test

This is the maintainer qualification for the `ip-reassign` strategy. It creates
disposable resources from `hack/e2e/aws`; it does not modify the platform
Terraform state. Run it from a connected workstation with AWS SSO, the cluster
API tunnel, `kubectl`, Helm, Terraform, Docker, Ansible, and `jq` available.

The topology qualified on 2026-08-17 was:

- region/AZ: `eu-west-1` / `eu-west-1a`
- Kubernetes subnet: `subnet-0f33760afe3396c4c`
- path A: `subnet-05bb741c35281cf0b`, `10.40.32.0/24`
- path B: `subnet-0b2eecd192a186fb6`, `10.40.36.0/24`
- existing worker: `i-0983ec5d63f718085`

The cluster was upgraded with Kubespray v2.31.0 to Kubernetes 1.34.7, then
Multus 4.2.2 was enabled. This Kubernetes minor upgrade is a breaking platform
change and belongs to the `iris-infra-kubernetes` 1.x release line; its 0.x
line remains on Kubernetes 1.33.

## 1. Provision the disposable substrate

Copy `terraform.tfvars.example` to ignored `terraform.tfvars`, fill the bastion
security group and the real IAM role ARN behind the SSO session, then run:

```bash
AWS_PROFILE=opsw_admin_rcs terraform -chdir=hack/e2e/aws init
AWS_PROFILE=opsw_admin_rcs terraform -chdir=hack/e2e/aws plan -out=.work/e2e.tfplan
AWS_PROFILE=opsw_admin_rcs terraform -chdir=hack/e2e/aws apply .work/e2e.tfplan
```

The module creates one temporary worker, a two-interface SCTP peer, two carrier
ENIs on both endpoint workers, the controller IAM role, and the test security
group. Its state and plan files are ignored but must be kept until cleanup.

## 2. Join the temporary worker

Render a temporary Kubespray inventory from the existing inventory:

```bash
worker=$(AWS_PROFILE=opsw_admin_rcs terraform -chdir=hack/e2e/aws output -json worker)
worker_dns=$(jq -r .private_dns <<<"$worker")
worker_ip=$(jq -r .private_ip <<<"$worker")
hack/e2e/render-inventory.sh \
  ../iris-infra-kubernetes/inventories/aws-kubespray/hosts.ini \
  .work/hosts.ini "$worker_dns" "$worker_ip"
```

From the pinned Kubespray checkout, add only the new node:

```bash
ansible-playbook -i ../anchor/.work/hosts.ini \
  -e @../iris-infra-kubernetes/generated/aws-staging/kubespray-vars.yml \
  -e kube_version=1.34.7 \
  -e kube_network_plugin_multus=true \
  -e multus_version=4.2.2 \
  -b scale.yml \
  --limit="$worker_dns"
```

Verify Kubernetes exposes `resource.k8s.io/v1`, both workers are Ready, Multus
is available, and `multus`, `ipvlan`, `static`, and `sbr` exist in the node CNI
binary directory.

## 3. Build and install Anchor

Build the controller/node image and test workload image for the worker
architecture. Import them into containerd on both endpoint workers when no
registry is used:

```bash
make image VERSION=0.1.0
docker build --platform linux/amd64 \
  -f hack/e2e/Dockerfile.sctp -t anchor-sctp-e2e:0.1.0 .
docker save -o .work/anchor-images.tar \
  anchor:0.1.0 anchor-sctp-e2e:0.1.0

ansible kube_node -i .work/hosts.ini --limit=ENDPOINT_NODES -b \
  -m copy -a 'src=.work/anchor-images.tar dest=/tmp/anchor-images.tar mode=0600'
ansible kube_node -i .work/hosts.ini --limit=ENDPOINT_NODES -b \
  -m shell -a 'ctr -n k8s.io images import /tmp/anchor-images.tar >/dev/null'
```

Label only the two workers with the test carrier ENIs. Create a short-lived
controller credential Secret, install the chart, and apply the examples:

```bash
kubectl --context rcs-staging label node ENDPOINT_NODE_1 ENDPOINT_NODE_2 \
  anchor.dra.example.com/enabled=true

KUBECTL_CONTEXT=rcs-staging AWS_PROFILE=opsw_admin_rcs \
  hack/e2e/assume-role-secret.sh \
  "$(AWS_PROFILE=opsw_admin_rcs terraform -chdir=hack/e2e/aws output -raw controller_role_arn)"

helm --kube-context rcs-staging upgrade --install anchor charts/anchor \
  --namespace anchor-system --create-namespace \
  --set aws.region=eu-west-1 \
  --set aws.credentialsSecretName=anchor-aws-credentials \
  --set image.repository=anchor --set image.tag=0.1.0 \
  --set image.pullPolicy=IfNotPresent

kubectl --context rcs-staging apply -f examples/aws-ip-reassign/deviceclass.yaml
kubectl --context rcs-staging apply -f examples/aws-ip-reassign/claim.yaml
kubectl --context rcs-staging apply -f examples/aws-ip-reassign/pod.yaml
kubectl --context rcs-staging -n anchor-e2e wait \
  --for=condition=Ready pod/smsc-sctp --timeout=180s
```

## 4. Verify placement and SCTP

Verify that two ResourceSlices exist, the claim is allocated and reserved, the
EndpointPlacement is Ready, and the pod has both addresses:

```bash
kubectl --context rcs-staging get resourceslices.resource.k8s.io
kubectl --context rcs-staging -n anchor-e2e get resourceclaim,endpointplacement
kubectl --context rcs-staging -n anchor-e2e exec smsc-sctp -- ip -d address
kubectl --context rcs-staging -n anchor-e2e exec smsc-sctp -- ip rule
```

Install `lksctp-tools` from the peer's configured OS repository and start a
multihomed listener:

```bash
sctp_test -H PEER_PATH_A_IP -B PEER_PATH_B_IP -P 2905 -l
```

Then send from both claimed addresses in the pod:

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
kubectl --context rcs-staging cordon CURRENT_ENDPOINT_NODE
kubectl --context rcs-staging -n anchor-e2e delete pod/smsc-sctp --wait
kubectl --context rcs-staging apply -f examples/aws-ip-reassign/pod.yaml
kubectl --context rcs-staging -n anchor-e2e wait \
  --for=condition=Ready pod/smsc-sctp --timeout=180s
kubectl --context rcs-staging uncordon CURRENT_ENDPOINT_NODE
```

The replacement must run on the other endpoint worker. Verify that both AWS
private addresses moved to that worker's tagged ENIs, the claim has no
`force-steal` annotation, and the same SCTP command establishes a new
association. On 2026-08-17, pod recreation through Ready took 7.73 seconds
with the default two-request-per-second global EC2 budget, excluding Kubernetes
node-failure detection.

## Evidence and cleanup

Record the following evidence under ignored `.work/evidence/`:

- node and system pod health before and after the Kubernetes upgrade
- ResourceClaim allocation and ResourceSlices
- EndpointPlacement state and Kubernetes events
- `ip -d address` and policy routes inside the pod
- EC2 private-IP ownership before and after rescheduling
- SCTP association and remote-path state proving both pod addresses are bound
  without NAT
- placement call count and latency

Before destroying Terraform, remove the temporary node with Kubespray
`remove-node.yml`. Destruction detaches and deletes only ENIs, instances,
security groups, and IAM resources held in the Anchor e2e state.

Never destroy the Terraform state before removing the temporary Kubernetes
node, and never delete carrier IP assignments as part of a Helm uninstall.
