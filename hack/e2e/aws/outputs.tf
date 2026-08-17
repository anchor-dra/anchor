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
