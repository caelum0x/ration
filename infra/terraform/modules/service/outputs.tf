output "service_name" {
  description = "ECS service name."
  value       = aws_ecs_service.this.name
}

output "service_id" {
  description = "ECS service ID (cluster/service ARN-ish identifier used by other AWS APIs)."
  value       = aws_ecs_service.this.id
}

output "task_definition_arn" {
  description = "ARN of the registered task definition (includes revision)."
  value       = aws_ecs_task_definition.this.arn
}

output "security_group_id" {
  description = "Security group ID attached to this service's task ENIs."
  value       = aws_security_group.this.id
}

output "log_group_name" {
  description = "CloudWatch log group name for this service's container logs."
  value       = aws_cloudwatch_log_group.this.name
}

output "target_group_arn" {
  description = "ALB target group ARN, if enable_alb is true; null otherwise."
  value       = var.enable_alb ? aws_lb_target_group.this[0].arn : null
}

output "service_discovery_arn" {
  description = "Cloud Map service ARN, if enable_service_discovery is true; null otherwise."
  value       = var.enable_service_discovery ? aws_service_discovery_service.this[0].arn : null
}

output "task_role_arn" {
  description = "IAM role ARN assumed by the running task (the service's own runtime permissions)."
  value       = aws_iam_role.task.arn
}

output "execution_role_arn" {
  description = "IAM role ARN ECS uses to pull the image, write logs, and read secrets."
  value       = aws_iam_role.execution.arn
}
