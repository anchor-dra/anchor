# IAM

The central controller needs read-only discovery plus one mutating API for the
alpha. Node plugins need no AWS identity.

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "Describe",
      "Effect": "Allow",
      "Action": [
        "ec2:DescribeInstances",
        "ec2:DescribeNetworkInterfaces",
        "ec2:DescribeSubnets"
      ],
      "Resource": "*"
    },
    {
      "Sid": "MutateTaggedCarrierENIs",
      "Effect": "Allow",
      "Action": [
        "ec2:AssignPrivateIpAddresses"
      ],
      "Resource": "arn:aws:ec2:REGION:ACCOUNT_ID:network-interface/*",
      "Condition": {
        "StringEquals": {
          "aws:ResourceTag/carrier-endpoint": "true"
        }
      }
    }
  ]
}
```

AWS supports resource-level authorization and resource-tag conditions for
`AssignPrivateIpAddresses`. Keep the mutation statement restricted to ENIs
tagged `carrier-endpoint=true`.

## Kubespray instance profile

For a self-managed cluster on EC2, attach the policy to the control-plane EC2
role and schedule `anchor-controller` only on those nodes. Set the control-plane
IMDSv2 response hop limit to `2`, keep tokens required, and install with
`aws.useInstanceProfile=true`.

This deliberately follows the node-role model used by AWS CCM and EBS CSI in
the Iris Kubespray platform. It is simpler than operating a self-managed OIDC
issuer, but every pod that can access control-plane IMDS shares the role. The
control-plane taint and tag-scoped mutation policy are therefore mandatory.
The node plugin must not run on the control-plane role for credential purposes
and does not call AWS APIs.

## EKS IRSA

EKS publishes the cluster OIDC issuer and performs the service-account token
injection. Terraform must register that issuer with IAM, create a role trusted
only by the Anchor controller service account, and attach the Anchor policy.

```hcl
terraform {
  required_providers {
    aws = {
      source = "hashicorp/aws"
    }
    tls = {
      source = "hashicorp/tls"
    }
  }
}

variable "cluster_name" {
  type = string
}

variable "aws_region" {
  type = string
}

data "aws_caller_identity" "current" {}

data "aws_partition" "current" {}

data "aws_eks_cluster" "this" {
  name = var.cluster_name
}

data "tls_certificate" "eks_oidc" {
  url = data.aws_eks_cluster.this.identity[0].oidc[0].issuer
}

locals {
  oidc_issuer      = data.aws_eks_cluster.this.identity[0].oidc[0].issuer
  oidc_provider_id = replace(local.oidc_issuer, "https://", "")
}

resource "aws_iam_openid_connect_provider" "eks" {
  url             = local.oidc_issuer
  client_id_list  = ["sts.amazonaws.com"]
  thumbprint_list = [data.tls_certificate.eks_oidc.certificates[0].sha1_fingerprint]
}

data "aws_iam_policy_document" "anchor_assume_role" {
  statement {
    actions = ["sts:AssumeRoleWithWebIdentity"]

    principals {
      type        = "Federated"
      identifiers = [aws_iam_openid_connect_provider.eks.arn]
    }

    condition {
      test     = "StringEquals"
      variable = "${local.oidc_provider_id}:aud"
      values   = ["sts.amazonaws.com"]
    }

    condition {
      test     = "StringEquals"
      variable = "${local.oidc_provider_id}:sub"
      values   = ["system:serviceaccount:anchor-system:anchor-controller"]
    }
  }
}

resource "aws_iam_role" "anchor_controller" {
  name               = "anchor-controller"
  assume_role_policy = data.aws_iam_policy_document.anchor_assume_role.json
}

data "aws_iam_policy_document" "anchor_controller" {
  statement {
    sid = "DescribeAnchorSubstrate"
    actions = [
      "ec2:DescribeInstances",
      "ec2:DescribeNetworkInterfaces",
      "ec2:DescribeSubnets"
    ]
    resources = ["*"]
  }

  statement {
    sid       = "MoveTaggedCarrierAddresses"
    actions   = ["ec2:AssignPrivateIpAddresses"]
    resources = [
      "arn:${data.aws_partition.current.partition}:ec2:${var.aws_region}:${data.aws_caller_identity.current.account_id}:network-interface/*"
    ]

    condition {
      test     = "StringEquals"
      variable = "aws:ResourceTag/carrier-endpoint"
      values   = ["true"]
    }
  }
}

resource "aws_iam_role_policy" "anchor_controller" {
  name   = "anchor-controller"
  role   = aws_iam_role.anchor_controller.id
  policy = data.aws_iam_policy_document.anchor_controller.json
}

output "anchor_controller_role_arn" {
  value = aws_iam_role.anchor_controller.arn
}
```

If the EKS OIDC provider already exists, reference it instead of creating a
duplicate. Annotate the controller service account through the chart:

```yaml
aws:
  region: eu-west-1
  useInstanceProfile: false
  credentialsSecretName: ""

controller:
  serviceAccount:
    annotations:
      eks.amazonaws.com/role-arn: arn:aws:iam::ACCOUNT_ID:role/anchor-controller
      eks.amazonaws.com/sts-regional-endpoints: "true"
```

The `sub` condition must match the Helm release name, namespace, and resulting
service-account name exactly. Do not annotate the Anchor node service account.

The short-lived Secret helper is retained only for disposable testing. Never
use it as a production credential source.
