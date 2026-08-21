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

variable "enable_route_repoint_harness" {
  type        = bool
  description = "Provision the disposable two-AZ TGW route-repoint qualification topology."
  default     = false
}

variable "route_repoint_az_b" {
  type    = string
  default = "eu-west-1b"
}

variable "route_repoint_kubernetes_subnet_b_id" {
  type    = string
  default = null
}

variable "route_repoint_carrier_subnets_b" {
  type = object({
    a = string
    b = string
  })
  default = null
}

variable "route_repoint_platform_tgw_subnet_cidrs" {
  type        = map(string)
  description = "Unused CIDRs in the platform VPC keyed by AZ for disposable TGW attachment subnets."
  default     = {}
}

variable "route_repoint_carrier_route_table_ids" {
  type        = set(string)
  description = "Route tables associated with carrier subnets; the harness adds the stable peer-VPC return prefix."
  default     = []
}

variable "route_repoint_peer_vpc_cidr" {
  type    = string
  default = "10.91.0.0/16"
}

variable "route_repoint_peer_subnet_cidrs" {
  type = map(string)
  default = {
    eu-west-1a = "10.91.1.0/24"
    eu-west-1b = "10.91.2.0/24"
  }
}

variable "route_repoint_carrier_prefix" {
  type        = string
  description = "External test prefix routed through TGW; it must not overlap either VPC."
  default     = "198.51.100.0/24"
}
