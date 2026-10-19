variable "aws_region" {
  description = "AWS region for this environment."
  type        = string
  default     = "us-east-1"
}

variable "azs" {
  description = "Availability zones for the prod VPC. Three, not two — prod is the one environment where losing an AZ's capacity outright (not just its NAT path) should not page anyone at 3am."
  type        = list(string)
  default     = ["us-east-1a", "us-east-1b", "us-east-1c"]
}

variable "image_tag" {
  description = "Image tag deployed for every service this environment wires up. CI sets this to a release tag/commit SHA that has already passed in staging — never \"latest\" for a real prod apply."
  type        = string
  default     = "latest"
}
