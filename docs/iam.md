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
        "ec2:AssignPrivateIpAddresses",
        "ec2:UnassignPrivateIpAddresses"
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

Use IRSA or EKS Pod Identity on EKS and generic workload OIDC elsewhere. The
short-lived Secret helper exists only for the self-managed staging proof. Do
not attach the mutating policy to a Kubernetes node role.
