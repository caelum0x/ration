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
  # with key = "prod/terraform.tfstate". Prod additionally wants the state
  # bucket's bucket policy to deny any principal outside the deploy role/
  # break-glass role from reading it (tenant Airwallex credentials'
  # Secrets Manager ARNs are plan-visible, not their values, but the ARNs
  # plus task definitions reveal the blast-radius topology).
}

provider "aws" {
  region = var.aws_region

  default_tags {
    tags = {
      Project     = "ration"
      Environment = "prod"
      ManagedBy   = "terraform"
    }
  }
}
