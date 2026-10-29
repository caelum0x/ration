# Ration prod environment — same wiring pattern as envs/dev, see that
# file's header comment for the section-1 service-split TODO (identical
# here, not repeated).
#
# Prod differs from dev/staging: 3 AZs, per-AZ NAT, ALB deletion
# protection, longer secret recovery window, higher gateway capacity +
# memory-based autoscaling in addition to CPU, immutable image tags.

locals {
  deployable_services = ["ledger-core", "gateway", "agent", "audit-log"]
  name                = "ration-prod"
  namespace_dns       = "ration.internal"
}

resource "aws_ecr_repository" "this" {
  for_each             = toset(local.deployable_services)
  name                 = "ration/${each.key}"
  image_tag_mutability = "IMMUTABLE"

  image_scanning_configuration {
    scan_on_push = true
  }
}

resource "aws_ecr_lifecycle_policy" "this" {
  for_each   = aws_ecr_repository.this
  repository = each.value.name

  policy = jsonencode({
    rules = [
      {
        rulePriority = 1
        description  = "expire untagged images after 14 days"
        selection = {
          tagStatus   = "untagged"
          countType   = "sinceImagePushed"
          countUnit   = "days"
          countNumber = 14
        }
        action = { type = "expire" }
      }
    ]
  })
}

module "vpc" {
  source = "../../modules/vpc"

  name               = local.name
  azs                = var.azs
  single_nat_gateway = false # prod: one NAT per AZ, no shared cross-AZ egress failure domain

  tags = { Project = "ration" }
}

resource "aws_service_discovery_private_dns_namespace" "internal" {
  name = local.namespace_dns
  vpc  = module.vpc.vpc_id
}

resource "aws_ecs_cluster" "this" {
  name = local.name

  setting {
    name  = "containerInsights"
    value = "enabled"
  }
}

resource "aws_security_group" "alb" {
  name        = "${local.name}-alb"
  description = "Ration prod ALB: public ingress on 80, egress to service tasks"
  vpc_id      = module.vpc.vpc_id

  ingress {
    description = "HTTP from the internet (no ACM cert/domain yet - see README)"
    from_port   = 80
    to_port     = 80
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

resource "aws_lb" "this" {
  name                       = "${local.name}-alb"
  internal                   = false
  load_balancer_type         = "application"
  security_groups            = [aws_security_group.alb.id]
  subnets                    = module.vpc.public_subnet_ids
  enable_deletion_protection = true
}

resource "aws_lb_listener" "http" {
  load_balancer_arn = aws_lb.this.arn
  port              = 80
  protocol          = "HTTP"

  default_action {
    type = "fixed-response"
    fixed_response {
      content_type = "text/plain"
      message_body = "no route"
      status_code  = "404"
    }
  }
}

resource "aws_secretsmanager_secret" "anthropic_api_key" {
  name                    = "${local.name}/ANTHROPIC_API_KEY"
  recovery_window_in_days = 30
}

resource "aws_secretsmanager_secret" "airwallex_api_key" {
  name                    = "${local.name}/AIRWALLEX_API_KEY"
  recovery_window_in_days = 30
}

resource "aws_secretsmanager_secret" "airwallex_webhook_secret" {
  name                    = "${local.name}/AIRWALLEX_WEBHOOK_SECRET"
  recovery_window_in_days = 30
}

module "ledger_backup" {
  source = "../../modules/ledger-backup"
  name   = local.name
  tags   = { Project = "ration" }
}

module "ledger_core" {
  source = "../../modules/service"

  name         = "ledger-core"
  environment  = "prod"
  cluster_arn  = aws_ecs_cluster.this.arn
  cluster_name = aws_ecs_cluster.this.name
  vpc_id       = module.vpc.vpc_id
  subnet_ids   = module.vpc.private_subnet_ids

  container_image      = "${aws_ecr_repository.this["ledger-core"].repository_url}:${var.image_tag}"
  container_port       = 7001
  cpu                  = 1024
  memory               = 2048
  ephemeral_storage_gb = 200

  desired_count = 1
  min_capacity  = 1
  max_capacity  = 1 # single-writer — see envs/dev/main.tf's note; this is the one place prod does NOT want more replicas

  # "Recreate" deployment semantics — see envs/dev/main.tf's
  # module.ledger_core comment for the full reasoning (default 100/200
  # lets old+new tasks run concurrently with disjoint on-disk state).
  # Most important to get right in prod of all three environments.
  deployment_minimum_healthy_percent = 0
  deployment_maximum_percent         = 100

  environment_variables = {
    RATION_LEDGER_WAL_PATH = "/data/ledger.wal"
    RATION_LEDGER_DB_PATH  = "/data/ledger.db"
    LEDGER_CORE_ADDR       = "0.0.0.0:7001"
    RUST_LOG               = "info"
    # ledger-core mirrors every ApplyEntry/RecordAgentDecision outcome
    # into audit-log over this address, best-effort and async (see
    # docs/SCALE-EXPANSION.md section 8, services/ledger-core/src/
    # audit_mirror.rs) — a down/unreachable audit-log never blocks or
    # fails ledger-core's own startup or RPCs.
    AUDIT_LOG_ADDR = "audit-log.${local.namespace_dns}:7003"
  }

  enable_service_discovery       = true
  service_discovery_namespace_id = aws_service_discovery_private_dns_namespace.internal.id

  allowed_security_group_ids = [
    module.gateway.security_group_id,
    module.agent.security_group_id,
  ]

  task_role_policy_arns = [module.ledger_backup.access_policy_arn]

  tags = { Project = "ration", Tier = "ledger" }
}

module "gateway" {
  source = "../../modules/service"

  name         = "gateway"
  environment  = "prod"
  cluster_arn  = aws_ecs_cluster.this.arn
  cluster_name = aws_ecs_cluster.this.name
  vpc_id       = module.vpc.vpc_id
  subnet_ids   = module.vpc.private_subnet_ids

  container_image = "${aws_ecr_repository.this["gateway"].repository_url}:${var.image_tag}"
  container_port  = 7002
  cpu             = 512
  memory          = 1024

  desired_count             = 3
  min_capacity              = 3
  max_capacity              = 12
  enable_memory_autoscaling = true

  environment_variables = {
    AIRWALLEX_MODE                      = "live"
    AIRWALLEX_BASE_URL                  = "https://api.airwallex.com"
    LEDGER_CORE_ADDR                    = "ledger-core.${local.namespace_dns}:7001"
    GATEWAY_GRPC_ADDR                   = "0.0.0.0:7002"
    GATEWAY_HTTP_ADDR                   = "0.0.0.0:8080"
    RATION_BRIDGE_MAX_FRACTION          = "0.6"
    RATION_BRIDGE_MIN_GRANT_MINOR_UNITS = "500"
    RATION_BRIDGE_VELOCITY_WINDOW_HOURS = "24"
    # Deliberately inert here, unlike module.ledger_core above:
    # ApplyEntry/RecordAgentDecision are both ledger-core RPCs, so every
    # call gateway makes to them is already mirrored once, centrally,
    # by ledger-core's own audit_mirror. Set anyway for the day
    # gateway's own card-issuance/webhook path needs a direct trail.
    AUDIT_LOG_ADDR = "audit-log.${local.namespace_dns}:7003"
  }

  secrets = {
    AIRWALLEX_API_KEY        = aws_secretsmanager_secret.airwallex_api_key.arn
    AIRWALLEX_WEBHOOK_SECRET = aws_secretsmanager_secret.airwallex_webhook_secret.arn
  }

  enable_service_discovery       = true
  service_discovery_namespace_id = aws_service_discovery_private_dns_namespace.internal.id

  allowed_security_group_ids = [
    module.agent.security_group_id,
  ]

  tags = { Project = "ration", Tier = "gateway" }
}

module "agent" {
  source = "../../modules/service"

  name         = "agent"
  environment  = "prod"
  cluster_arn  = aws_ecs_cluster.this.arn
  cluster_name = aws_ecs_cluster.this.name
  vpc_id       = module.vpc.vpc_id
  subnet_ids   = module.vpc.private_subnet_ids

  container_image = "${aws_ecr_repository.this["agent"].repository_url}:${var.image_tag}"
  container_port  = null
  cpu             = 512
  memory          = 1024

  desired_count = 1
  min_capacity  = 1
  max_capacity  = 1

  environment_variables = {
    LEDGER_CORE_ADDR                   = "ledger-core.${local.namespace_dns}:7001"
    GATEWAY_GRPC_ADDR                  = "gateway.${local.namespace_dns}:7002"
    RATION_AGENT_POLL_INTERVAL_SECONDS = "10"
    RATION_AGENT_MODEL                 = "claude-sonnet-5"
  }

  secrets = {
    ANTHROPIC_API_KEY = aws_secretsmanager_secret.anthropic_api_key.arn
  }

  tags = { Project = "ration", Tier = "agent" }
}

# --- audit-log (Go, gRPC :7003) — see envs/dev/main.tf's module.audit_log
#     for the full rationale (separate trust boundary, single-writer hash
#     chain, same Fargate ephemeral-storage caveat as ledger-core with no
#     snapshot-backup module yet — a real, scoped gap). ---
module "audit_log" {
  source = "../../modules/service"

  name         = "audit-log"
  environment  = "prod"
  cluster_arn  = aws_ecs_cluster.this.arn
  cluster_name = aws_ecs_cluster.this.name
  vpc_id       = module.vpc.vpc_id
  subnet_ids   = module.vpc.private_subnet_ids

  container_image = "${aws_ecr_repository.this["audit-log"].repository_url}:${var.image_tag}"
  container_port  = 7003
  cpu             = 512
  memory          = 1024

  desired_count = 1
  min_capacity  = 1
  max_capacity  = 1 # single-writer hash chain — see envs/dev/main.tf's note; this is the one place prod does NOT want more replicas

  # "Recreate" deployment semantics — see envs/dev/main.tf's
  # module.audit_log comment for the full reasoning.
  deployment_minimum_healthy_percent = 0
  deployment_maximum_percent         = 100

  environment_variables = {
    AUDIT_LOG_GRPC_ADDR       = "0.0.0.0:7003"
    RATION_AUDIT_LOG_DATA_DIR = "/data"
  }

  enable_service_discovery       = true
  service_discovery_namespace_id = aws_service_discovery_private_dns_namespace.internal.id

  allowed_security_group_ids = [
    module.ledger_core.security_group_id,
    module.gateway.security_group_id,
  ]

  tags = { Project = "ration", Tier = "audit" }
}

# See envs/dev/main.tf's trailing comment block for the exact section-1
# service-split TODO — identical mechanical steps apply here.
