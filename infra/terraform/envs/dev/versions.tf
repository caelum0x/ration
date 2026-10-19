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

  # No backend block: this environment plans against local state for now.
  #
  # TODO(owner): real deployment wants a remote backend —
  #   backend "s3" {
  #     bucket         = "ration-terraform-state-<account-id>"
  #     key            = "dev/terraform.tfstate"
  #     region         = "us-east-1"
  #     dynamodb_table = "ration-terraform-locks"
  #     encrypt        = true
  #   }
  # Exact blocker: that S3 bucket + DynamoDB lock table must exist before
  # `terraform init` can use them as a backend (classic chicken-and-egg).
  # Bootstrap order for whoever has real AWS creds: apply a one-off
  # bootstrap config (bucket + versioning + table) with local state, THEN
  # add the backend block above and run `terraform init -migrate-state`.
}

provider "aws" {
  region = var.aws_region

  default_tags {
    tags = {
      Project     = "ration"
      Environment = "dev"
      ManagedBy   = "terraform"
    }
  }
}
