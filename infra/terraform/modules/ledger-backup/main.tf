# S3 bucket for periodic ledger-core snapshot backups.
#
# docs/SCALE-EXPANSION.md section 3 is explicit about what this is NOT:
# ledger-core's write-ahead log stays on local/EBS disk at the instance —
# the WAL is the durability source of truth and S3 is not in its write
# path. This bucket exists only for periodic snapshot backups (a
# scheduled job outside Terraform's scope — see the TODO in
# envs/dev/main.tf — pushes a point-in-time snapshot of ledger-core's
# SQLite index + a WAL checkpoint here) so a full instance/EBS loss is
# recoverable from an out-of-band backup, not just from the WAL.

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
}

locals {
  tags = merge(var.tags, {
    ManagedBy = "terraform"
    Purpose   = "ledger-core-snapshot-backup"
  })
}

# S3 bucket names are globally unique across all AWS accounts; a random
# suffix avoids a plan-time collision with another account's bucket.
resource "random_id" "suffix" {
  byte_length = 4
}

resource "aws_s3_bucket" "this" {
  bucket = "${var.name}-ledger-backup-${random_id.suffix.hex}"
  tags   = local.tags
}

resource "aws_s3_bucket_versioning" "this" {
  bucket = aws_s3_bucket.this.id
  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_public_access_block" "this" {
  bucket                  = aws_s3_bucket.this.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_server_side_encryption_configuration" "this" {
  bucket = aws_s3_bucket.this.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "aws:kms"
    }
    bucket_key_enabled = true
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "this" {
  bucket = aws_s3_bucket.this.id

  rule {
    id     = "snapshot-lifecycle"
    status = "Enabled"

    filter {}

    transition {
      days          = var.transition_to_glacier_days
      storage_class = "GLACIER_IR"
    }

    noncurrent_version_expiration {
      noncurrent_days = var.noncurrent_version_expiration_days
    }
  }
}

# Customer-managed policy, scoped to this bucket only — pass the output
# ARN into the ledger-core service module's task_role_policy_arns so
# ledger-core's task role (and only that role) can push snapshots here.
data "aws_iam_policy_document" "access" {
  statement {
    sid = "ListBucket"
    actions = [
      "s3:ListBucket",
    ]
    resources = [aws_s3_bucket.this.arn]
  }

  statement {
    sid = "ReadWriteObjects"
    actions = [
      "s3:GetObject",
      "s3:PutObject",
    ]
    resources = ["${aws_s3_bucket.this.arn}/*"]
  }
}

resource "aws_iam_policy" "access" {
  name   = "${var.name}-ledger-backup-access"
  policy = data.aws_iam_policy_document.access.json
  tags   = local.tags
}
