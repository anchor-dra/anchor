output "worker" {
  value = {
    id          = aws_instance.worker.id
    private_ip  = aws_instance.worker.private_ip
    private_dns = aws_instance.worker.private_dns
  }
}

output "peer" {
  value = {
    id        = aws_instance.peer.id
    path_a_ip = var.peer_path_a_ip
    path_b_ip = var.peer_path_b_ip
  }
}

output "controller_role_arn" { value = aws_iam_role.controller.arn }
output "carrier_eni_ids" { value = { for name, eni in aws_network_interface.carrier : name => eni.id } }

output "route_repoint" {
  value = var.enable_route_repoint_harness ? {
    carrier_prefix          = var.route_repoint_carrier_prefix
    managed_route_table_ids = sort([for table in aws_route_table.route_ingress : table.id])
    managed_route_table_tag = { key = "anchor-managed", value = "true" }
    peer_instance_id        = aws_instance.route_peer[0].id
    peer_private_ip         = aws_instance.route_peer[0].private_ip
    worker_b                = { id = aws_instance.route_worker_b[0].id, private_dns = aws_instance.route_worker_b[0].private_dns }
    worker_b_carrier_enis   = { for name, eni in aws_network_interface.route_worker_b : name => eni.id }
    stable_routes = {
      tgw_carrier_aggregate = aws_ec2_transit_gateway_route.carrier_aggregate[0].id
      peer_return_prefix    = var.route_repoint_peer_vpc_cidr
    }
  } : null
}

output "anchor_route_repoint" {
  description = "Production-compatible topology handoff for route-repoint rendering and preflight."
  value = var.enable_route_repoint_harness ? {
    vpc_id                  = var.vpc_id
    vpc_cidr                = data.aws_vpc.platform[0].cidr_block
    managed_route_table_ids = sort([for table in aws_route_table.route_ingress : table.id])
    managed_route_table_tag = { key = "anchor-managed", value = "true" }
    carrier_interfaces = merge(
      {
        for name, eni in aws_network_interface.carrier : name => {
          id        = eni.id
          subnet_id = eni.subnet_id
          path      = local.carrier_paths[name].path
        }
      },
      {
        for name, eni in aws_network_interface.route_worker_b : "route_worker_b_${name}" => {
          id        = eni.id
          subnet_id = eni.subnet_id
          path      = name
        }
      },
    )
  } : null
}
