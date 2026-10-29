# Ration dev environment — wires infra/terraform/modules/service per
# deployable service. See docs/SCALE-EXPANSION.md section 3.
#
# IMPORTANT — section 1 (service decomposition) has NOT shipped yet.
# docs/SCALE-EXPANSION.md section 3 describes wiring auth,
# tenant-registry, card-issuance, and webhook-ingestion as their own
# services, but those are services/gateway's *planned* split (section 1)
# and none of services/auth, services/tenant-registry,
# services/card-issuance, services/webhook-ingestion exist in this repo
# yet — there is nothing to containerize or wire for them. This file
# wires the three deployable services that actually exist today
# (ledger-core, gateway, agent) with the exact module call pattern below;
# see the commented block at the bottom for the precise, mechanical
# addition each of the four split services needs once section 1 ships.

locals {
  # Services with their own image to build/push/run. tui and cli are
  # interactive operator clients (bubbletea TUI, cobra CLI) — not long-
  # running network services — so they get Dockerfiles (see repo root)
  # for a consistent `docker run -it` developer experience, but they are
  # deliberately NOT wired into ECS here: there is no "deployed instance"
  # of a terminal a human drives interactively.
  deployable_services = ["ledger-core", "gateway", "agent", "audit-log"]

  name          = "ration-dev"
  namespace_dns = "ration.internal"
}

# --- Container registry ---------------------------------------------------

resource "aws_ecr_repository" "this" {
  for_each             = toset(local.deployable_services)
  name                 = "ration/${each.key}"
  image_tag_mutability = "MUTABLE"

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

# --- Network ---------------------------------------------------------------

module "vpc" {
  source = "../../modules/vpc"

  name               = local.name
  azs                = var.azs
  single_nat_gateway = true # dev: cost over AZ-redundant egress

  tags = { Project = "ration" }
}

resource "aws_service_discovery_private_dns_namespace" "internal" {
  name = local.namespace_dns
  vpc  = module.vpc.vpc_id
}

# --- ECS cluster -------------------------------------------------------

resource "aws_ecs_cluster" "this" {
  name = local.name

  setting {
    name  = "containerInsights"
    value = "enabled"
  }
}

# --- Shared ALB (provisioned now, routed to once section 7's REST facade
#     ships — see docs/SCALE-EXPANSION.md section 7). Keeping the ALB and
#     its listener here means section 7 only needs to add
#     aws_lb_listener_rule resources (via enable_alb=true on the gateway
#     module call), not re-plumb the VPC. ---

resource "aws_security_group" "alb" {
  name        = "${local.name}-alb"
  description = "Ration dev ALB: public ingress on 80, egress to service tasks"
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
  name               = "${local.name}-alb"
  internal           = false
  load_balancer_type = "application"
  security_groups    = [aws_security_group.alb.id]
  subnets            = module.vpc.public_subnet_ids
}

# Default action is a fixed 404: every real route is an explicit listener
# rule (aws_lb_listener_rule) attached by a service module call with
# enable_alb=true, so an unmatched path fails closed instead of silently
# hitting whichever service happened to register first.
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

  # TODO(owner, has a real domain): add an HTTPS listener here once ACM
  # has a certificate for a real domain + Route53 has a hosted zone — the
  # doc explicitly calls out Route53 as optional/commented for exactly
  # this reason (no domain registered yet). Redirect this HTTP listener's
  # default action to that HTTPS listener once it exists.
}

# --- Secrets (metadata only — see modules/service/variables.tf's `secrets`
#     docstring: the owner populates the real value out of band, once per
#     environment, never via a committed .tfvars file) ---

resource "aws_secretsmanager_secret" "anthropic_api_key" {
  name                    = "${local.name}/ANTHROPIC_API_KEY"
  recovery_window_in_days = 0 # dev: allow instant recreate, no 7-30 day hold
}

resource "aws_secretsmanager_secret" "airwallex_api_key" {
  name                    = "${local.name}/AIRWALLEX_API_KEY"
  recovery_window_in_days = 0
}

resource "aws_secretsmanager_secret" "airwallex_webhook_secret" {
  name                    = "${local.name}/AIRWALLEX_WEBHOOK_SECRET"
  recovery_window_in_days = 0
}

# --- Ledger snapshot backup -----------------------------------------------

module "ledger_backup" {
  source = "../../modules/ledger-backup"
  name   = local.name
  tags   = { Project = "ration" }
}

# --- ledger-core (Rust, gRPC :7001) ---------------------------------------
#
# Single-writer by design (services/ledger-core owns all tenant/ledger
# state through one WAL) — pinned at exactly 1 task, never horizontally
# scaled; running N replicas would mean N independent WALs/SQLite indexes
# with no coordination, which is a correctness bug, not a capacity win.
#
# ephemeral_storage_gb=200 (max) is this environment's honest answer to
# "where does the WAL live on Fargate" — it is still ephemeral scratch
# disk (a task replacement starts with an empty one), which is why
# module.ledger_backup exists: see infra/terraform/README.md's
# "ledger-core on Fargate" section for the accepted gap and the real
# migration path (ECS-on-EC2 + EBS, or Fargate-attached EBS volumes) if
# this needs to be airtight before real money moves through it.
module "ledger_core" {
  source = "../../modules/service"

  name         = "ledger-core"
  environment  = "dev"
  cluster_arn  = aws_ecs_cluster.this.arn
  cluster_name = aws_ecs_cluster.this.name
  vpc_id       = module.vpc.vpc_id
  subnet_ids   = module.vpc.private_subnet_ids

  container_image      = "${aws_ecr_repository.this["ledger-core"].repository_url}:${var.image_tag}"
  container_port       = 7001
  cpu                  = 512
  memory               = 1024
  ephemeral_storage_gb = 200

  desired_count = 1
  min_capacity  = 1
  max_capacity  = 1 # see note above: single-writer, not horizontally scaled

  # "Recreate" deployment semantics, not rolling: stop the old task
  # fully before starting the new one. The module's own default
  # (100/200) lets ECS run the old AND new task concurrently for the
  # health-check window of every deploy — harmless for a stateless
  # service, but for this single-writer, local-WAL-only service it
  # means two processes with completely disjoint on-disk state (the
  # new task's ephemeral storage starts empty, nothing replicates the
  # old task's WAL to it) both answer tenant-scoped RPCs under the
  # same Cloud Map name until the old one is torn down — and any write
  # ECS happens to route to the old task in that window is lost the
  # moment it's stopped. A brief deploy-time gap is the correct
  # tradeoff here, not split-brain on real money state.
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

# --- gateway (Go, gRPC :7002 [+ HTTP :8080 webhooks when AIRWALLEX_MODE=live]) ---
#
# enable_alb=false here deliberately: the gRPC surface this module call
# exposes is the internal gateway.proto contract the agent/tui/cli dial
# directly over service discovery. The REST/JSON facade in
# docs/SCALE-EXPANSION.md section 7 is what the ALB (provisioned above)
# is for — once that facade exists, flip enable_alb=true on this call and
# add alb_listener_arn/alb_security_group_id/alb_priority/path_pattern,
# no other plumbing needed.
module "gateway" {
  source = "../../modules/service"

  name         = "gateway"
  environment  = "dev"
  cluster_arn  = aws_ecs_cluster.this.arn
  cluster_name = aws_ecs_cluster.this.name
  vpc_id       = module.vpc.vpc_id
  subnet_ids   = module.vpc.private_subnet_ids

  container_image = "${aws_ecr_repository.this["gateway"].repository_url}:${var.image_tag}"
  container_port  = 7002
  cpu             = 256
  memory          = 512

  desired_count = 1
  min_capacity  = 1
  max_capacity  = 3

  environment_variables = {
    AIRWALLEX_MODE                      = "mock" # flip to "live" + populate secrets below for a real Airwallex sandbox/prod run
    AIRWALLEX_BASE_URL                  = "https://api-demo.airwallex.com"
    LEDGER_CORE_ADDR                    = "ledger-core.${local.namespace_dns}:7001"
    GATEWAY_GRPC_ADDR                   = "0.0.0.0:7002"
    GATEWAY_HTTP_ADDR                   = "0.0.0.0:8080"
    RATION_BRIDGE_MAX_FRACTION          = "0.6"
    RATION_BRIDGE_MIN_GRANT_MINOR_UNITS = "500"
    RATION_BRIDGE_VELOCITY_WINDOW_HOURS = "24"
    # Deliberately inert here, unlike module.ledger_core above:
    # ApplyEntry/RecordAgentDecision are both ledger-core RPCs — every
    # call gateway makes to them (there is no other path to move
    # money or record a decision, see docs/ARCHITECTURE.md) already
    # gets mirrored once, centrally, by ledger-core's own audit_mirror.
    # Set here anyway for the day gateway's card-issuance/webhook path
    # needs its own direct audit trail entries (e.g. an Airwallex
    # webhook gateway chooses not to forward into ApplyEntry at all).
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

# --- agent (Go, no inbound listener — polls ledger-core/gateway) ---------

module "agent" {
  source = "../../modules/service"

  name         = "agent"
  environment  = "dev"
  cluster_arn  = aws_ecs_cluster.this.arn
  cluster_name = aws_ecs_cluster.this.name
  vpc_id       = module.vpc.vpc_id
  subnet_ids   = module.vpc.private_subnet_ids

  container_image = "${aws_ecr_repository.this["agent"].repository_url}:${var.image_tag}"
  container_port  = null # no server: cmd/agent/main.go never listens on a socket
  cpu             = 256
  memory          = 512

  desired_count = 1
  min_capacity  = 1
  max_capacity  = 1 # one poll loop; sharding for throughput is new product code, not an infra change (see docs/RUNBOOK.md TODO)

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

# --- audit-log (Go, gRPC :7003) ------------------------------------------
#
# Append-only, hash-chained audit trail (see proto/audit.proto,
# docs/THREAT-MODEL.md) — a deliberately separate trust boundary from
# ledger-core: a compromised card-issuance/gateway process can't rewrite
# history it already mirrored here. Internal-only (no ALB route, nothing
# public dials this directly).
#
# Pinned at 1 task for the same reason as ledger-core above: it is a
# single local-disk, hash-chained append log, not a horizontally
# scalable stateless service — N replicas would mean N independent,
# uncoordinated chains. It inherits the exact same Fargate
# ephemeral-storage caveat documented in infra/terraform/README.md's
# "ledger-core on Fargate" section (a task replacement starts with an
# empty disk); unlike ledger-core it has no S3 snapshot-backup module
# yet — flagged here as a real, scoped gap, not silently accepted,
# because audit-log's entire value proposition is surviving exactly the
# kind of incident that causes a task replacement.
module "audit_log" {
  source = "../../modules/service"

  name         = "audit-log"
  environment  = "dev"
  cluster_arn  = aws_ecs_cluster.this.arn
  cluster_name = aws_ecs_cluster.this.name
  vpc_id       = module.vpc.vpc_id
  subnet_ids   = module.vpc.private_subnet_ids

  container_image = "${aws_ecr_repository.this["audit-log"].repository_url}:${var.image_tag}"
  container_port  = 7003
  cpu             = 256
  memory          = 512

  desired_count = 1
  min_capacity  = 1
  max_capacity  = 1 # single-writer hash chain — see note above

  # Same "recreate, not rolling" reasoning as module.ledger_core above:
  # audit-log is also single-writer, local-disk-only, with no
  # replication between tasks, so the old+new overlap the module's
  # default (100/200) would otherwise allow is a real split-brain risk
  # here too.
  deployment_minimum_healthy_percent = 0
  deployment_maximum_percent         = 100

  environment_variables = {
    AUDIT_LOG_GRPC_ADDR       = "0.0.0.0:7003"
    RATION_AUDIT_LOG_DATA_DIR = "/data"
  }

  enable_service_discovery       = true
  service_discovery_namespace_id = aws_service_discovery_private_dns_namespace.internal.id

  # ledger-core is the one real caller today (see the AUDIT_LOG_ADDR
  # note on module.ledger_core above) — gateway's security group is
  # allowed too since its AUDIT_LOG_ADDR var is set for the same
  # reason (see module.gateway's note), even though nothing calls out
  # on it yet.
  allowed_security_group_ids = [
    module.ledger_core.security_group_id,
    module.gateway.security_group_id,
  ]

  tags = { Project = "ration", Tier = "audit" }
}

# ---------------------------------------------------------------------------
# TODO(section 1 service split, exact mechanical addition once it ships):
#
# For each of auth, tenant-registry, card-issuance, webhook-ingestion:
#   1. infra/terraform/envs/dev/main.tf: add
#        resource "aws_ecr_repository" "this" { for_each = toset([...]) }
#      — just add the name to `local.deployable_services` above, the
#      for_each already picks it up.
#   2. Add a Dockerfile at the new services/<name>/Dockerfile (same
#      multi-stage pattern as services/gateway/Dockerfile).
#   3. Add a `module "<name>" { source = "../../modules/service" ... }`
#      block identical in shape to module.gateway above: internal-only
#      (enable_service_discovery=true, enable_alb=false) for
#      auth/tenant-registry/card-issuance; webhook-ingestion is the one
#      exception — it receives inbound Airwallex webhooks, so it is the
#      actual enable_alb=true candidate once section 7's REST facade (or
#      just its own public path) exists, NOT gateway.
#   4. Wire allowed_security_group_ids on whichever of these + ledger-core
#      + gateway the new service calls, mirroring the pattern above.
#   5. services/gateway becomes a thin BFF calling these four over
#      service discovery DNS (<name>.ration.internal:<port>) instead of
#      owning their logic directly — update module.gateway's
#      environment_variables accordingly in the same pass that does the
#      Go code split.
# ---------------------------------------------------------------------------
