# Reusable ECS Fargate service module — see docs/SCALE-EXPANSION.md section 3.
#
# One instantiation of this module = one independently deployable Ration
# service (ledger-core, gateway, agent today; auth, tenant-registry,
# card-issuance, webhook-ingestion once docs/SCALE-EXPANSION.md section 1's
# gateway split ships — see envs/dev/main.tf for the exact TODO).

variable "name" {
  description = "Short service name, e.g. \"gateway\", \"ledger-core\". Used in resource names/tags."
  type        = string
}

variable "environment" {
  description = "Deployment environment: dev, staging, or prod."
  type        = string

  validation {
    condition     = contains(["dev", "staging", "prod"], var.environment)
    error_message = "environment must be one of: dev, staging, prod."
  }
}

variable "cluster_arn" {
  description = "ARN of the shared ECS cluster this service deploys into."
  type        = string
}

variable "cluster_name" {
  description = "Name of the shared ECS cluster (needed separately from the ARN for the autoscaling resource_id format)."
  type        = string
}

variable "vpc_id" {
  description = "VPC ID the service's security group is created in."
  type        = string
}

variable "subnet_ids" {
  description = "Subnets the ECS service's ENIs are placed in (private subnets in every real environment)."
  type        = list(string)
}

variable "assign_public_ip" {
  description = "Whether ECS assigns a public IP to task ENIs. Must be true if subnet_ids are public subnets with no NAT; false for private-subnet-with-NAT deployments (the norm for staging/prod)."
  type        = bool
  default     = false
}

variable "container_image" {
  description = "Full image reference (registry/repo:tag or digest) to run."
  type        = string
}

variable "container_port" {
  description = "Port the service's container listens on (gRPC or HTTP). Leave null for a service with no inbound listener at all (e.g. services/agent, a pure background poller) — no portMappings entry and no ingress rule are created, so the task's security group stays fully closed to inbound traffic."
  type        = number
  default     = null
}

variable "cpu" {
  description = "Fargate task-level vCPU units (256, 512, 1024, ...)."
  type        = number
  default     = 256
}

variable "memory" {
  description = "Fargate task-level memory in MiB."
  type        = number
  default     = 512
}

variable "ephemeral_storage_gb" {
  description = <<-EOT
    Fargate task ephemeral storage size in GiB (21-200; AWS default is 20
    if unset). NOTE: this is still ephemeral per-task scratch disk, not a
    durable volume — a task replacement (deploy, AZ failure, scheduler
    eviction) gets a *fresh empty disk*. For ledger-core specifically this
    is a real, documented gap: see infra/terraform/README.md's
    "ledger-core on Fargate" section for why this project accepts it for
    now (periodic S3 snapshot backup as the recovery path) and the real
    migration path (ECS-on-EC2 or Fargate EBS-backed volumes) once ledger
    durability under task replacement needs to be airtight.
  EOT
  type        = number
  default     = null
}

variable "desired_count" {
  description = "Initial desired task count. Ignored after the first apply if autoscaling is enabled (the autoscaler owns desired_count from then on)."
  type        = number
  default     = 1
}

variable "min_capacity" {
  description = "Autoscaling floor for task count."
  type        = number
  default     = 1
}

variable "max_capacity" {
  description = "Autoscaling ceiling for task count."
  type        = number
  default     = 4
}

variable "cpu_target_value" {
  description = "Target average CPU utilization (%) the autoscaler tracks."
  type        = number
  default     = 60
}

variable "enable_memory_autoscaling" {
  description = "Also attach a target-tracking policy on average memory utilization, in addition to the mandatory CPU policy."
  type        = bool
  default     = false
}

variable "memory_target_value" {
  description = "Target average memory utilization (%) the autoscaler tracks, if enable_memory_autoscaling is true."
  type        = number
  default     = 70
}

variable "environment_variables" {
  description = "Plain (non-secret) environment variables injected into the container."
  type        = map(string)
  default     = {}
}

variable "secrets" {
  description = <<-EOT
    Secret environment variables, name => Secrets Manager secret ARN (or
    ARN:json-key:: for a single key inside a JSON secret). The task
    execution role is granted secretsmanager:GetSecretValue scoped to
    exactly these ARNs — nothing broader. Values themselves are never set
    here; see envs/*/secrets.tf, which creates the secret *metadata* only
    and leaves populating the real value to the owner (aws secretsmanager
    put-secret-value, run once per environment, out of band and never
    committed).
  EOT
  type        = map(string)
  default     = {}
}

variable "log_retention_in_days" {
  description = "CloudWatch Logs retention for this service's log group."
  type        = number
  default     = 30
}

variable "allowed_security_group_ids" {
  description = "Security group IDs allowed to reach container_port directly (service-to-service gRPC callers, e.g. gateway/agent calling ledger-core)."
  type        = list(string)
  default     = []
}

variable "allowed_cidr_blocks" {
  description = "CIDR blocks allowed to reach container_port directly, in addition to allowed_security_group_ids (rarely needed; prefer security groups)."
  type        = list(string)
  default     = []
}

# --- ALB wiring (public-facing services only, e.g. gateway's REST facade) ---

variable "enable_alb" {
  description = "Attach this service to a shared ALB via a target group + listener rule."
  type        = bool
  default     = false
}

variable "alb_listener_arn" {
  description = "ARN of the shared ALB listener to attach a routing rule to. Required if enable_alb is true."
  type        = string
  default     = null
}

variable "alb_security_group_id" {
  description = "Security group ID of the shared ALB, granted ingress to container_port. Required if enable_alb is true."
  type        = string
  default     = null
}

variable "alb_priority" {
  description = "Listener rule priority (must be unique per listener across all services). Required if enable_alb is true."
  type        = number
  default     = null
}

variable "path_pattern" {
  description = "ALB listener rule path-pattern condition values, e.g. [\"/api/*\"]. Required if enable_alb is true."
  type        = list(string)
  default     = []
}

variable "health_check_path" {
  description = "ALB target group health check path."
  type        = string
  default     = "/health"
}

variable "health_check_protocol_version" {
  description = "Target group protocol_version: HTTP1 for the REST facade, GRPC for a target group health-checking a gRPC service directly (uses gRPC Health Checking Protocol semantics, status code range 0-0)."
  type        = string
  default     = "HTTP1"

  validation {
    condition     = contains(["HTTP1", "HTTP2", "GRPC"], var.health_check_protocol_version)
    error_message = "health_check_protocol_version must be one of: HTTP1, HTTP2, GRPC."
  }
}

variable "health_check_matcher" {
  description = "Expected status code(s) for a healthy target. \"200\" for REST, \"0-0\" for GRPC target groups (gRPC maps OK to HTTP2 status 0)."
  type        = string
  default     = "200"
}

# --- Cloud Map service discovery (internal-only gRPC services) ---

variable "enable_service_discovery" {
  description = "Register this service in Cloud Map for DNS-based east-west discovery (ledger-core, agent, and internal services that have no public ALB route)."
  type        = bool
  default     = false
}

variable "service_discovery_namespace_id" {
  description = "Cloud Map private DNS namespace ID. Required if enable_service_discovery is true."
  type        = string
  default     = null
}

# --- IAM ---

variable "task_role_policy_arns" {
  description = "Additional managed policy ARNs attached to the task role (the service's own runtime permissions, e.g. S3 access for ledger-backup)."
  type        = list(string)
  default     = []
}

variable "tags" {
  description = "Tags applied to every resource this module creates."
  type        = map(string)
  default     = {}
}
