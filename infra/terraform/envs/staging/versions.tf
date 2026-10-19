terraform {
  required_version = ">= 1.5.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 5.0"
    }
    random = {
      source  = "hashicorp/random"
      version = ">= 3.0"
    }
  }

  # TODO(owner): same remote-backend bootstrap note as envs/dev/versions.tf,
  # with key = "staging/terraform.tfstate".
}

provider "aws" {
  region = var.aws_region

  default_tags {
    tags = {
      Project     = "ration"
      Environment = "staging"
      ManagedBy   = "terraform"
    }
  }
}
