output "bucket_name" {
  value = aws_s3_bucket.this.bucket
}

output "bucket_arn" {
  value = aws_s3_bucket.this.arn
}

output "access_policy_arn" {
  description = "IAM policy ARN granting read/write to exactly this bucket. Attach to ledger-core's task role via the service module's task_role_policy_arns."
  value       = aws_iam_policy.access.arn
}
