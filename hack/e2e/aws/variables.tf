variable "aws_region" {
  type    = string
  default = "eu-west-1"
}

variable "availability_zone" {
  type    = string
  default = "eu-west-1a"
}

variable "cluster_name" {
  type    = string
  default = "anchor-e2e"
}

variable "vpc_id" { type = string }
variable "kubernetes_subnet_id" { type = string }
variable "carrier_subnet_a_id" { type = string }
variable "carrier_subnet_b_id" { type = string }
variable "existing_worker_instance_id" { type = string }
variable "worker_ami_id" { type = string }
variable "worker_key_name" { type = string }
variable "worker_instance_profile_name" { type = string }
variable "worker_security_group_ids" { type = list(string) }
variable "bastion_security_group_id" { type = string }
variable "trusted_operator_role_arn" {
  type        = string
  description = "IAM role ARN used by the operator's SSO session, not an STS assumed-role ARN."
}

variable "worker_instance_type" {
  type    = string
  default = "t3.2xlarge"
}

variable "peer_instance_type" {
  type    = string
  default = "t3.small"
}

variable "peer_path_a_ip" {
  type    = string
  default = "10.50.1.20"
}

variable "peer_path_b_ip" {
  type    = string
  default = "10.50.2.20"
}
