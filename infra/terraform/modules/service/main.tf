locals {
  full_name = "${var.environment}-${var.name}"

  tags = merge(var.tags, {
    Service     = var.name
    Environment = var.environment
    ManagedBy   = "terraform"
  })

  secret_arns = values(var.secrets)
}

# --- CloudWatch log group -----------------------------------------------

resource "aws_cloudwatch_log_group" "this" {
  name              = "/ecs/${local.full_name}"
  retention_in_days = var.log_retention_in_days
  tags              = local.tags
}

# --- IAM: task execution role (pulls image, writes logs, reads secrets) --

data "aws_iam_policy_document" "ecs_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ecs-tasks.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "execution" {
  name               = "${local.full_name}-exec"
  assume_role_policy = data.aws_iam_policy_document.ecs_assume.json
  tags               = local.tags
}

resource "aws_iam_role_policy_attachment" "execution_managed" {
  role       = aws_iam_role.execution.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}

# Scoped to exactly the secret ARNs this service was given — never a
# wildcard "secretsmanager:*" grant.
data "aws_iam_policy_document" "secrets_access" {
  count = length(local.secret_arns) > 0 ? 1 : 0

  statement {
    actions   = ["secretsmanager:GetSecretValue"]
    resources = [for arn in local.secret_arns : "${arn}*"]
  }
}

resource "aws_iam_role_policy" "execution_secrets" {
  count  = length(local.secret_arns) > 0 ? 1 : 0
  name   = "${local.full_name}-secrets"
  role   = aws_iam_role.execution.id
  policy = data.aws_iam_policy_document.secrets_access[0].json
}

# --- IAM: task role (the service's own runtime permissions) -------------

resource "aws_iam_role" "task" {
  name               = "${local.full_name}-task"
  assume_role_policy = data.aws_iam_policy_document.ecs_assume.json
  tags               = local.tags
}

resource "aws_iam_role_policy_attachment" "task_extra" {
  for_each   = toset(var.task_role_policy_arns)
  role       = aws_iam_role.task.name
  policy_arn = each.value
}

# --- Security group -------------------------------------------------------

resource "aws_security_group" "this" {
  name        = "${local.full_name}-svc"
  description = "Ration ${var.name} (${var.environment}): ECS task ENI security group"
  vpc_id      = var.vpc_id
  tags        = local.tags

  egress {
    description = "allow all outbound (ledger-core/Secrets Manager/ECR/Airwallex/Anthropic API calls)"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

resource "aws_security_group_rule" "from_alb" {
  count                    = var.enable_alb ? 1 : 0
  type                     = "ingress"
  security_group_id        = aws_security_group.this.id
  from_port                = var.container_port
  to_port                  = var.container_port
  protocol                 = "tcp"
  source_security_group_id = var.alb_security_group_id
  description              = "ALB -> ${var.name}"
}

resource "aws_security_group_rule" "from_peers" {
  for_each                 = toset(var.allowed_security_group_ids)
  type                     = "ingress"
  security_group_id        = aws_security_group.this.id
  from_port                = var.container_port
  to_port                  = var.container_port
  protocol                 = "tcp"
  source_security_group_id = each.value
  description              = "peer service -> ${var.name}"
}

resource "aws_security_group_rule" "from_cidrs" {
  count             = length(var.allowed_cidr_blocks) > 0 ? 1 : 0
  type              = "ingress"
  security_group_id = aws_security_group.this.id
  from_port         = var.container_port
  to_port           = var.container_port
  protocol          = "tcp"
  cidr_blocks       = var.allowed_cidr_blocks
  description       = "explicit CIDR allowlist -> ${var.name}"
}

# --- Task definition -------------------------------------------------------

resource "aws_ecs_task_definition" "this" {
  family                   = local.full_name
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = var.cpu
  memory                   = var.memory
  execution_role_arn       = aws_iam_role.execution.arn
  task_role_arn            = aws_iam_role.task.arn
  tags                     = local.tags

  dynamic "ephemeral_storage" {
    for_each = var.ephemeral_storage_gb == null ? [] : [var.ephemeral_storage_gb]
    content {
      size_in_gib = ephemeral_storage.value
    }
  }

  container_definitions = jsonencode([
    {
      name      = var.name
      image     = var.container_image
      essential = true
      portMappings = var.container_port == null ? [] : [
        {
          containerPort = var.container_port
          protocol      = "tcp"
        }
      ]
      environment = [
        for k, v in var.environment_variables : { name = k, value = v }
      ]
      secrets = [
        for k, arn in var.secrets : { name = k, valueFrom = arn }
      ]
      logConfiguration = {
        logDriver = "awslogs"
        options = {
          "awslogs-group"         = aws_cloudwatch_log_group.this.name
          "awslogs-region"        = data.aws_region.current.region
          "awslogs-stream-prefix" = var.name
        }
      }
    }
  ])
}

data "aws_region" "current" {}

# --- ALB target group + listener rule (public-facing services only) ------

resource "aws_lb_target_group" "this" {
  count                = var.enable_alb ? 1 : 0
  name                 = "${local.full_name}-tg"
  port                 = var.container_port
  protocol             = var.health_check_protocol_version == "GRPC" ? "HTTP2" : "HTTP"
  protocol_version     = var.health_check_protocol_version
  vpc_id               = var.vpc_id
  target_type          = "ip"
  deregistration_delay = 30

  health_check {
    path                = var.health_check_path
    matcher             = var.health_check_matcher
    interval            = 15
    timeout             = 5
    healthy_threshold   = 2
    unhealthy_threshold = 3
  }

  tags = local.tags
}

resource "aws_lb_listener_rule" "this" {
  count        = var.enable_alb ? 1 : 0
  listener_arn = var.alb_listener_arn
  priority     = var.alb_priority

  action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.this[0].arn
  }

  condition {
    path_pattern {
      values = var.path_pattern
    }
  }

  tags = local.tags
}

# --- Cloud Map service discovery (internal-only services) ----------------

resource "aws_service_discovery_service" "this" {
  count = var.enable_service_discovery ? 1 : 0
  name  = var.name

  dns_config {
    namespace_id = var.service_discovery_namespace_id

    dns_records {
      ttl  = 10
      type = "A"
    }

    routing_policy = "MULTIVALUE"
  }

  # No health_check_custom_config block: its only argument
  # (failure_threshold) is deprecated — AWS always treats it as 1 now —
  # and ECS already drives this service's Cloud Map health status
  # automatically via service_registries, no custom health check needed.

  tags = local.tags
}

# --- ECS service -----------------------------------------------------------

resource "aws_ecs_service" "this" {
  name            = local.full_name
  cluster         = var.cluster_arn
  task_definition = aws_ecs_task_definition.this.arn
  desired_count   = var.desired_count
  launch_type     = "FARGATE"

  network_configuration {
    subnets          = var.subnet_ids
    security_groups  = [aws_security_group.this.id]
    assign_public_ip = var.assign_public_ip
  }

  dynamic "load_balancer" {
    for_each = var.enable_alb ? [1] : []
    content {
      target_group_arn = aws_lb_target_group.this[0].arn
      container_name   = var.name
      container_port   = var.container_port
    }
  }

  dynamic "service_registries" {
    for_each = var.enable_service_discovery ? [1] : []
    content {
      registry_arn = aws_service_discovery_service.this[0].arn
    }
  }

  deployment_minimum_healthy_percent = var.deployment_minimum_healthy_percent
  deployment_maximum_percent         = var.deployment_maximum_percent

  tags = local.tags

  # Once the autoscaling target below exists, it (not this resource) owns
  # desired_count — without this, every `terraform apply` would fight the
  # autoscaler back to var.desired_count.
  lifecycle {
    ignore_changes = [desired_count]
  }

  depends_on = [aws_iam_role_policy_attachment.execution_managed]
}

# --- Autoscaling -----------------------------------------------------------

resource "aws_appautoscaling_target" "this" {
  max_capacity       = var.max_capacity
  min_capacity       = var.min_capacity
  resource_id        = "service/${var.cluster_name}/${aws_ecs_service.this.name}"
  scalable_dimension = "ecs:service:DesiredCount"
  service_namespace  = "ecs"
}

resource "aws_appautoscaling_policy" "cpu" {
  name               = "${local.full_name}-cpu"
  policy_type        = "TargetTrackingScaling"
  resource_id        = aws_appautoscaling_target.this.resource_id
  scalable_dimension = aws_appautoscaling_target.this.scalable_dimension
  service_namespace  = aws_appautoscaling_target.this.service_namespace

  target_tracking_scaling_policy_configuration {
    predefined_metric_specification {
      predefined_metric_type = "ECSServiceAverageCPUUtilization"
    }
    target_value       = var.cpu_target_value
    scale_in_cooldown  = 120
    scale_out_cooldown = 60
  }
}

resource "aws_appautoscaling_policy" "memory" {
  count              = var.enable_memory_autoscaling ? 1 : 0
  name               = "${local.full_name}-memory"
  policy_type        = "TargetTrackingScaling"
  resource_id        = aws_appautoscaling_target.this.resource_id
  scalable_dimension = aws_appautoscaling_target.this.scalable_dimension
  service_namespace  = aws_appautoscaling_target.this.service_namespace

  target_tracking_scaling_policy_configuration {
    predefined_metric_specification {
      predefined_metric_type = "ECSServiceAverageMemoryUtilization"
    }
    target_value       = var.memory_target_value
    scale_in_cooldown  = 120
    scale_out_cooldown = 60
  }
}
