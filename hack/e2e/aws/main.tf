data "aws_subnet" "kubernetes" { id = var.kubernetes_subnet_id }
data "aws_subnet" "carrier_a" { id = var.carrier_subnet_a_id }
data "aws_subnet" "carrier_b" { id = var.carrier_subnet_b_id }

locals {
  carrier_paths = {
    existing_a  = { subnet_id = var.carrier_subnet_a_id, path = "a", instance_id = var.existing_worker_instance_id, device_index = 1 }
    existing_b  = { subnet_id = var.carrier_subnet_b_id, path = "b", instance_id = var.existing_worker_instance_id, device_index = 2 }
    temporary_a = { subnet_id = var.carrier_subnet_a_id, path = "a", instance_id = aws_instance.worker.id, device_index = 1 }
    temporary_b = { subnet_id = var.carrier_subnet_b_id, path = "b", instance_id = aws_instance.worker.id, device_index = 2 }
  }
}

resource "aws_instance" "worker" {
  ami                    = var.worker_ami_id
  instance_type          = var.worker_instance_type
  subnet_id              = var.kubernetes_subnet_id
  key_name               = var.worker_key_name
  iam_instance_profile   = var.worker_instance_profile_name
  vpc_security_group_ids = var.worker_security_group_ids

  root_block_device {
    volume_type = "gp3"
    volume_size = 100
    encrypted   = true
  }

  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 2
  }

  tags = {
    Name                                        = "anchor-e2e-worker"
    component                                   = "kubespray-worker"
    kubespray-role                              = "kube_node"
    tenant                                      = "shared"
    "kubernetes.io/cluster/${var.cluster_name}" = "owned"
  }

  lifecycle {
    precondition {
      condition     = data.aws_subnet.kubernetes.vpc_id == var.vpc_id && data.aws_subnet.kubernetes.availability_zone == var.availability_zone
      error_message = "Kubernetes test subnet must be in the selected VPC and AZ."
    }
  }
}

resource "aws_security_group" "carrier" {
  name_prefix = "anchor-e2e-carrier-"
  description = "Carrier ENIs and SCTP e2e peer."
  vpc_id      = var.vpc_id
}

resource "aws_vpc_security_group_ingress_rule" "sctp" {
  security_group_id            = aws_security_group.carrier.id
  referenced_security_group_id = aws_security_group.carrier.id
  ip_protocol                  = "132"
  description                  = "SCTP between endpoint and peer paths."
}

resource "aws_vpc_security_group_ingress_rule" "ssh" {
  security_group_id            = aws_security_group.carrier.id
  referenced_security_group_id = var.bastion_security_group_id
  ip_protocol                  = "tcp"
  from_port                    = 22
  to_port                      = 22
  description                  = "Operator SSH through the existing bastion."
}

resource "aws_vpc_security_group_egress_rule" "all" {
  security_group_id = aws_security_group.carrier.id
  ip_protocol       = "-1"
  cidr_ipv4         = "0.0.0.0/0"
}

resource "aws_network_interface" "carrier" {
  for_each          = local.carrier_paths
  subnet_id         = each.value.subnet_id
  security_groups   = [aws_security_group.carrier.id]
  source_dest_check = false
  tags = {
    Name             = "anchor-e2e-${each.key}"
    carrier-endpoint = "true"
    carrier-path     = each.value.path
    anchor-e2e       = "true"
  }

  lifecycle {
    precondition {
      condition     = data.aws_subnet.carrier_a.vpc_id == var.vpc_id && data.aws_subnet.carrier_b.vpc_id == var.vpc_id && data.aws_subnet.carrier_a.availability_zone == var.availability_zone && data.aws_subnet.carrier_b.availability_zone == var.availability_zone
      error_message = "Both carrier subnets must be in the selected VPC and AZ."
    }
  }
}

resource "aws_network_interface_attachment" "carrier" {
  for_each             = local.carrier_paths
  instance_id          = each.value.instance_id
  network_interface_id = aws_network_interface.carrier[each.key].id
  device_index         = each.value.device_index
}

resource "aws_network_interface" "peer_a" {
  subnet_id       = var.carrier_subnet_a_id
  private_ips     = [var.peer_path_a_ip]
  security_groups = [aws_security_group.carrier.id]
  tags            = { Name = "anchor-e2e-peer-a", anchor-e2e = "true" }
}

resource "aws_network_interface" "peer_b" {
  subnet_id       = var.carrier_subnet_b_id
  private_ips     = [var.peer_path_b_ip]
  security_groups = [aws_security_group.carrier.id]
  tags            = { Name = "anchor-e2e-peer-b", anchor-e2e = "true" }
}

resource "aws_instance" "peer" {
  ami                  = var.worker_ami_id
  instance_type        = var.peer_instance_type
  key_name             = var.worker_key_name
  iam_instance_profile = var.worker_instance_profile_name

  network_interface {
    network_interface_id = aws_network_interface.peer_a.id
    device_index         = 0
  }

  network_interface {
    network_interface_id = aws_network_interface.peer_b.id
    device_index         = 1
  }

  root_block_device {
    volume_type = "gp3"
    volume_size = 20
    encrypted   = true
  }

  metadata_options {
    http_endpoint = "enabled"
    http_tokens   = "required"
  }

  tags = { Name = "anchor-e2e-sctp-peer" }
}

data "aws_iam_policy_document" "controller_trust" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "AWS"
      identifiers = [var.trusted_operator_role_arn]
    }
  }
}

resource "aws_iam_role" "controller" {
  name_prefix        = "anchor-e2e-controller-"
  assume_role_policy = data.aws_iam_policy_document.controller_trust.json
}

data "aws_iam_policy_document" "controller" {
  statement {
    sid = "Describe"
    actions = [
      "ec2:DescribeInstances",
      "ec2:DescribeNetworkInterfaces",
      "ec2:DescribeRouteTables",
      "ec2:DescribeVpcs",
      "ec2:DescribeSubnets"
    ]
    resources = ["*"]
  }

  statement {
    sid       = "MutateTaggedCarrierENIs"
    actions   = ["ec2:AssignPrivateIpAddresses", "ec2:UnassignPrivateIpAddresses"]
    resources = ["arn:aws:ec2:${var.aws_region}:*:network-interface/*"]
    condition {
      test     = "StringEquals"
      variable = "aws:ResourceTag/carrier-endpoint"
      values   = ["true"]
    }
  }

  dynamic "statement" {
    for_each = var.enable_route_repoint_harness ? [1] : []
    content {
      sid     = "RepointQualifiedCarrierRoutes"
      actions = ["ec2:CreateRoute", "ec2:ReplaceRoute"]
      resources = [
        for table in aws_route_table.route_ingress : table.arn
      ]
      condition {
        test     = "StringEquals"
        variable = "aws:ResourceTag/anchor-managed"
        values   = ["true"]
      }
    }
  }
}

resource "aws_iam_role_policy" "controller" {
  name   = "anchor-ip-reassign"
  role   = aws_iam_role.controller.id
  policy = data.aws_iam_policy_document.controller.json
}
