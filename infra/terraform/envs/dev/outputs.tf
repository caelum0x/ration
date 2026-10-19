output "vpc_id" {
  value = module.vpc.vpc_id
}

output "ecs_cluster_name" {
  value = aws_ecs_cluster.this.name
}

output "alb_dns_name" {
  value = aws_lb.this.dns_name
}

output "ecr_repository_urls" {
  value = { for k, repo in aws_ecr_repository.this : k => repo.repository_url }
}

output "ledger_core_service_name" {
  value = module.ledger_core.service_name
}

output "gateway_service_name" {
  value = module.gateway.service_name
}

output "agent_service_name" {
  value = module.agent.service_name
}

output "audit_log_service_name" {
  value = module.audit_log.service_name
}
