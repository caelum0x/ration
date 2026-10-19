# Ration infra (Terraform)

Status: **plan-only**. `terraform apply` against any of these environments
is an owner action — it needs real AWS credentials and spends real money
(NAT gateways, ALB, Fargate tasks, ECR storage all bill hourly/per-GB
regardless of traffic). The verifiable artifact this repo ships is
`terraform plan`, not `terraform apply`. Do not run apply here.

```
infra/terraform/
├── modules/
│   ├── service/         # reusable ECS Fargate service module
│   ├── vpc/              # shared VPC (public+private subnets, NAT, IGW)
│   └── ledger-backup/    # S3 bucket + scoped IAM policy for ledger-core snapshots
└── envs/
    ├── dev/
    ├── staging/
    └── prod/
```

## What's wired today vs. what's a TODO

`envs/{dev,staging,prod}/main.tf` each wire four deployable services —
`ledger-core`, `gateway`, `agent`, `audit-log` — because those are the
four services in this repo that actually have a server to run.
`docs/SCALE-EXPANSION.md` section 3 describes wiring six-plus (adding
`auth`, `tenant-registry`, `card-issuance`, `webhook-ingestion`), but
those are section 1's gateway split, which has not shipped — there is no
`services/auth` etc. directory to containerize yet. Each env's `main.tf`
ends with the exact, mechanical steps to add those four once section 1
ships; nothing here needs re-architecting to add them, just more
`module` blocks of the same shape.

`audit-log`'s `AUDIT_LOG_ADDR` env var is set on both `module.ledger_core`
and `module.gateway` in every env, but only `ledger-core` actually dials
it: `services/ledger-core/src/audit_mirror.rs` mirrors every
`ApplyEntry`/`RecordAgentDecision` outcome there, best-effort and async
(see `docs/SCALE-EXPANSION.md` section 8). `module.gateway`'s copy of the
var is deliberately inert — `ApplyEntry`/`RecordAgentDecision` are both
ledger-core RPCs, so every call gateway makes to them is already mirrored
once, centrally, by ledger-core; gateway's own var is set so the network
path/env var are ready the day gateway's card-issuance or webhook path
needs a direct audit-log client of its own. Separately, `audit-log`
inherits the exact same Fargate ephemeral-storage caveat as `ledger-core`
(below) but has no `ledger-backup`-equivalent S3 snapshot module yet —
flagged, not silently accepted.

`services/tui` and `services/cli` are interactive operator tools (a
bubbletea TUI, a cobra CLI) a human runs against a deployed environment —
not long-running network services. They get Dockerfiles (for a consistent
`docker run -it` dev experience) but are intentionally not wired into ECS.

## ledger-core on Fargate: the honest tradeoff

`services/ledger-core` is a single-writer process: all tenant/ledger state
goes through one write-ahead log on local disk (`services/wal`), replayed
on boot. AWS Fargate does not give a task a durable disk by default —
`ephemeral_storage` (set to the 200GiB max in every env here) is still
*ephemeral*: a task replacement (a deploy, an AZ event, the scheduler
evicting the task for any reason) starts with an empty disk. ledger-core's
WAL on that disk would be gone.

This repo's accepted answer for now: `module.ledger_backup` provisions an
S3 bucket for periodic point-in-time snapshots (SQLite index + a WAL
checkpoint) as the recovery path if a task is ever rescheduled onto fresh
storage. **The scheduled job that actually produces those snapshots does
not exist yet** — that is application code (a `ration-snapshot` sidecar
or a `cmd/ledger-snapshot` binary calling into `ledger-core`'s own
recovery logic to produce a consistent point-in-time copy), not a
Terraform change, and is an explicit TODO, not a silent gap: without it,
`module.ledger_backup`'s bucket stays empty and a task replacement is a
real, unrecovered data-loss event.

The real production migration path, when ledger durability under task
replacement needs to be airtight (i.e. before this moves real money):
either (a) move `ledger-core` to ECS-on-EC2 with an attached EBS volume
that survives task replacement on the same instance plus EBS snapshots
for cross-instance recovery, or (b) use Fargate's EBS-volume-attachment
support (`aws_ecs_service.volume_configuration` with a `managed_ebs_volume`
block, GA since late 2024) instead of `ephemeral_storage`. Both are a
real, scoped follow-up, not implemented in this pass — flagging it here
rather than letting the 200GiB ephemeral_storage value read as "this is
fine."

## Remote state

Every env currently plans against local state (no `backend` block). Each
`envs/*/versions.tf` has the exact backend config + bootstrap order for
whoever has real AWS credentials to turn this into a proper remote
backend (S3 + DynamoDB lock table) — it is a chicken-and-egg problem
(the backend's own storage must exist before `terraform init` can use it
as a backend), explicitly called out rather than glossed over.

## Secrets

`aws_secretsmanager_secret` resources here create secret **metadata**
only — never a real value. Populate each environment's secrets once,
out of band, after a real `apply`:

```
aws secretsmanager put-secret-value \
  --secret-id ration-dev/ANTHROPIC_API_KEY \
  --secret-string "sk-ant-..."
```

Never put a real secret value in a `.tfvars` file or anywhere Terraform
would write it into state in plaintext-adjacent form.

## Running `validate` / `plan` without real credentials

`terraform validate` only checks configuration syntax/internal
consistency — no AWS credentials needed. `terraform plan` against the AWS
provider *does* need credentials (even read-only ones) to look up
default/data values and diff against real state; with no credentials
configured, `plan` fails at the provider-auth step, which is the expected,
correctly-reported outcome in this sandbox (see the plan output captured
when this was built — it's a provider auth error, not a configuration
bug). A real dry-run plan is something only the owner, with read-only AWS
credentials, can produce.
