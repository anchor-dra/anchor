locals {
  route_repoint_enabled_map = var.enable_route_repoint_harness ? var.route_repoint_platform_tgw_subnet_cidrs : {}
  route_worker_b_paths = var.enable_route_repoint_harness && var.route_repoint_carrier_subnets_b != null ? {
    a = { subnet_id = var.route_repoint_carrier_subnets_b.a, device_index = 1 }
    b = { subnet_id = var.route_repoint_carrier_subnets_b.b, device_index = 2 }
  } : {}
}

data "aws_vpc" "platform" {
  count = var.enable_route_repoint_harness ? 1 : 0
  id    = var.vpc_id
}

resource "aws_subnet" "route_tgw_ingress" {
  for_each = local.route_repoint_enabled_map

  vpc_id            = var.vpc_id
  availability_zone = each.key
  cidr_block        = each.value
  tags = {
    Name       = "anchor-e2e-tgw-ingress-${each.key}"
    anchor-e2e = "true"
  }

  lifecycle {
    precondition {
      condition     = !var.enable_route_repoint_harness || length(var.route_repoint_platform_tgw_subnet_cidrs) >= 2
      error_message = "Route-repoint qualification requires TGW ingress subnets in at least two AZs."
    }
  }
}

resource "aws_route_table" "route_ingress" {
  for_each = aws_subnet.route_tgw_ingress
  vpc_id   = var.vpc_id
  tags = {
    Name           = "anchor-e2e-route-ingress-${each.key}"
    anchor-e2e     = "true"
    anchor-managed = "true"
  }
}

resource "aws_route_table_association" "route_ingress" {
  for_each       = aws_subnet.route_tgw_ingress
  subnet_id      = each.value.id
  route_table_id = aws_route_table.route_ingress[each.key].id
}

resource "aws_vpc" "route_peer" {
  count                = var.enable_route_repoint_harness ? 1 : 0
  cidr_block           = var.route_repoint_peer_vpc_cidr
  enable_dns_support   = true
  enable_dns_hostnames = true
  tags                 = { Name = "anchor-e2e-route-peer", anchor-e2e = "true" }
}

resource "aws_subnet" "route_peer" {
  for_each = var.enable_route_repoint_harness ? var.route_repoint_peer_subnet_cidrs : {}

  vpc_id            = aws_vpc.route_peer[0].id
  availability_zone = each.key
  cidr_block        = each.value
  tags              = { Name = "anchor-e2e-route-peer-${each.key}", anchor-e2e = "true" }
}

resource "aws_route_table" "route_peer" {
  for_each = aws_subnet.route_peer
  vpc_id   = aws_vpc.route_peer[0].id
  tags     = { Name = "anchor-e2e-route-peer-${each.key}", anchor-e2e = "true" }
}

resource "aws_route_table_association" "route_peer" {
  for_each       = aws_subnet.route_peer
  subnet_id      = each.value.id
  route_table_id = aws_route_table.route_peer[each.key].id
}

resource "aws_ec2_transit_gateway" "route" {
  count                           = var.enable_route_repoint_harness ? 1 : 0
  description                     = "Disposable Anchor route-repoint qualification TGW"
  default_route_table_association = "disable"
  default_route_table_propagation = "disable"
  tags                            = { Name = "anchor-e2e-route", anchor-e2e = "true" }
}

resource "aws_ec2_transit_gateway_route_table" "route" {
  count              = var.enable_route_repoint_harness ? 1 : 0
  transit_gateway_id = aws_ec2_transit_gateway.route[0].id
  tags               = { Name = "anchor-e2e-route", anchor-e2e = "true" }
}

resource "aws_ec2_transit_gateway_vpc_attachment" "platform" {
  count              = var.enable_route_repoint_harness ? 1 : 0
  subnet_ids         = values(aws_subnet.route_tgw_ingress)[*].id
  transit_gateway_id = aws_ec2_transit_gateway.route[0].id
  vpc_id             = var.vpc_id
  tags               = { Name = "anchor-e2e-platform", anchor-e2e = "true" }
}

resource "aws_ec2_transit_gateway_vpc_attachment" "peer" {
  count              = var.enable_route_repoint_harness ? 1 : 0
  subnet_ids         = values(aws_subnet.route_peer)[*].id
  transit_gateway_id = aws_ec2_transit_gateway.route[0].id
  vpc_id             = aws_vpc.route_peer[0].id
  tags               = { Name = "anchor-e2e-peer", anchor-e2e = "true" }
}

resource "aws_ec2_transit_gateway_route_table_association" "platform" {
  count                          = var.enable_route_repoint_harness ? 1 : 0
  transit_gateway_attachment_id  = aws_ec2_transit_gateway_vpc_attachment.platform[0].id
  transit_gateway_route_table_id = aws_ec2_transit_gateway_route_table.route[0].id
}

resource "aws_ec2_transit_gateway_route_table_association" "peer" {
  count                          = var.enable_route_repoint_harness ? 1 : 0
  transit_gateway_attachment_id  = aws_ec2_transit_gateway_vpc_attachment.peer[0].id
  transit_gateway_route_table_id = aws_ec2_transit_gateway_route_table.route[0].id
}

resource "aws_ec2_transit_gateway_route" "carrier_aggregate" {
  count                          = var.enable_route_repoint_harness ? 1 : 0
  destination_cidr_block         = var.route_repoint_carrier_prefix
  transit_gateway_attachment_id  = aws_ec2_transit_gateway_vpc_attachment.platform[0].id
  transit_gateway_route_table_id = aws_ec2_transit_gateway_route_table.route[0].id
}

resource "aws_ec2_transit_gateway_route" "peer" {
  count                          = var.enable_route_repoint_harness ? 1 : 0
  destination_cidr_block         = var.route_repoint_peer_vpc_cidr
  transit_gateway_attachment_id  = aws_ec2_transit_gateway_vpc_attachment.peer[0].id
  transit_gateway_route_table_id = aws_ec2_transit_gateway_route_table.route[0].id
}

resource "aws_route" "peer_to_carrier" {
  for_each               = aws_route_table.route_peer
  route_table_id         = each.value.id
  destination_cidr_block = var.route_repoint_carrier_prefix
  transit_gateway_id     = aws_ec2_transit_gateway.route[0].id
}

resource "aws_route" "carrier_return" {
  for_each               = var.enable_route_repoint_harness ? var.route_repoint_carrier_route_table_ids : []
  route_table_id         = each.value
  destination_cidr_block = var.route_repoint_peer_vpc_cidr
  transit_gateway_id     = aws_ec2_transit_gateway.route[0].id
}

resource "aws_instance" "route_worker_b" {
  count                  = var.enable_route_repoint_harness ? 1 : 0
  ami                    = var.worker_ami_id
  instance_type          = var.worker_instance_type
  subnet_id              = var.route_repoint_kubernetes_subnet_b_id
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
    Name                                        = "anchor-e2e-route-worker-b"
    component                                   = "kubespray-worker"
    kubespray-role                              = "kube_node"
    anchor-e2e                                  = "true"
    "kubernetes.io/cluster/${var.cluster_name}" = "owned"
  }

  lifecycle {
    precondition {
      condition     = !var.enable_route_repoint_harness || var.route_repoint_kubernetes_subnet_b_id != null
      error_message = "Route-repoint qualification requires a Kubernetes subnet in the second AZ."
    }
  }
}

resource "aws_network_interface" "route_worker_b" {
  for_each          = local.route_worker_b_paths
  subnet_id         = each.value.subnet_id
  security_groups   = [aws_security_group.carrier.id]
  source_dest_check = false
  tags              = { Name = "anchor-e2e-route-worker-b-${each.key}", carrier-endpoint = "true", carrier-path = each.key, anchor-e2e = "true" }
}

resource "aws_network_interface_attachment" "route_worker_b" {
  for_each             = aws_network_interface.route_worker_b
  instance_id          = aws_instance.route_worker_b[0].id
  network_interface_id = each.value.id
  device_index         = local.route_worker_b_paths[each.key].device_index
}

resource "aws_security_group" "route_peer" {
  count       = var.enable_route_repoint_harness ? 1 : 0
  name_prefix = "anchor-e2e-route-peer-"
  vpc_id      = aws_vpc.route_peer[0].id
  tags        = { anchor-e2e = "true" }
}

resource "aws_vpc_security_group_ingress_rule" "route_peer_carrier" {
  count             = var.enable_route_repoint_harness ? 1 : 0
  security_group_id = aws_security_group.route_peer[0].id
  ip_protocol       = "-1"
  cidr_ipv4         = var.route_repoint_carrier_prefix
}

resource "aws_vpc_security_group_egress_rule" "route_peer" {
  count             = var.enable_route_repoint_harness ? 1 : 0
  security_group_id = aws_security_group.route_peer[0].id
  ip_protocol       = "-1"
  cidr_ipv4         = "0.0.0.0/0"
}

resource "aws_instance" "route_peer" {
  count                  = var.enable_route_repoint_harness ? 1 : 0
  ami                    = var.worker_ami_id
  instance_type          = var.peer_instance_type
  subnet_id              = values(aws_subnet.route_peer)[0].id
  iam_instance_profile   = var.worker_instance_profile_name
  vpc_security_group_ids = [aws_security_group.route_peer[0].id]
  root_block_device {
    volume_type = "gp3"
    volume_size = 20
    encrypted   = true
  }
  metadata_options {
    http_endpoint = "enabled"
    http_tokens   = "required"
  }
  user_data = <<-EOT
    #!/bin/bash
    set -eu
    install -d -m 0755 /var/lib/anchor-e2e
    printf 'anchor route-repoint peer\n' >/var/lib/anchor-e2e/index.html
    cd /var/lib/anchor-e2e
    nohup python3 -m http.server 8080 --bind 0.0.0.0 >/var/log/anchor-e2e-peer.log 2>&1 &
  EOT
  tags      = { Name = "anchor-e2e-route-peer", anchor-e2e = "true" }
}
