variable "name" {
  description = "Name prefix for all VPC resources, e.g. \"ration-dev\"."
  type        = string
}

variable "cidr_block" {
  description = "CIDR block for the VPC."
  type        = string
  default     = "10.20.0.0/16"
}

variable "azs" {
  description = "Availability zones to spread public/private subnets across."
  type        = list(string)
}

variable "single_nat_gateway" {
  description = "If true, create one NAT gateway (cheaper, single AZ failure domain for egress — fine for dev). If false, one NAT gateway per AZ (prod posture: a NAT outage in one AZ doesn't take down the others)."
  type        = bool
  default     = true
}

variable "tags" {
  description = "Tags applied to every resource this module creates."
  type        = map(string)
  default     = {}
}
