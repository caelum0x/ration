variable "aws_region" {
  description = "AWS region for this environment."
  type        = string
  default     = "us-east-1"
}

variable "azs" {
  description = "Availability zones for the staging VPC."
  type        = list(string)
  default     = ["us-east-1a", "us-east-1b"]
}

variable "image_tag" {
  description = "Image tag deployed for every service this environment wires up. Set per-deploy by CI to a commit SHA; defaults to \"latest\" for manual plans."
  type        = string
  default     = "latest"
}
