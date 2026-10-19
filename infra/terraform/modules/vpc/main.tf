# Shared VPC module — docs/SCALE-EXPANSION.md section 3: "a shared VPC
# module". Deliberately minimal (one VPC, public+private subnets per AZ,
# IGW, NAT) rather than pulling in terraform-aws-modules/vpc: this repo has
# no real traffic shaping/peering requirements yet, and a hand-rolled
# module keeps `terraform plan`'s output small enough to actually read.

terraform {
  required_version = ">= 1.5.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 5.0"
    }
  }
}

locals {
  tags = merge(var.tags, {
    ManagedBy = "terraform"
  })
}

resource "aws_vpc" "this" {
  cidr_block           = var.cidr_block
  enable_dns_support   = true
  enable_dns_hostnames = true

  tags = merge(local.tags, { Name = var.name })
}

resource "aws_internet_gateway" "this" {
  vpc_id = aws_vpc.this.id
  tags   = merge(local.tags, { Name = "${var.name}-igw" })
}

# /20 per subnet out of the /16 — plenty of room (4094 hosts) per AZ for
# ECS task ENIs (awsvpc mode means every task consumes an IP).
resource "aws_subnet" "public" {
  for_each = { for idx, az in var.azs : az => idx }

  vpc_id                  = aws_vpc.this.id
  availability_zone       = each.key
  cidr_block              = cidrsubnet(var.cidr_block, 4, each.value)
  map_public_ip_on_launch = true

  tags = merge(local.tags, { Name = "${var.name}-public-${each.key}", Tier = "public" })
}

resource "aws_subnet" "private" {
  for_each = { for idx, az in var.azs : az => idx }

  vpc_id            = aws_vpc.this.id
  availability_zone = each.key
  cidr_block        = cidrsubnet(var.cidr_block, 4, each.value + length(var.azs))

  tags = merge(local.tags, { Name = "${var.name}-private-${each.key}", Tier = "private" })
}

resource "aws_eip" "nat" {
  for_each = var.single_nat_gateway ? toset([var.azs[0]]) : toset(var.azs)
  domain   = "vpc"

  tags = merge(local.tags, { Name = "${var.name}-nat-${each.key}" })
}

resource "aws_nat_gateway" "this" {
  for_each = var.single_nat_gateway ? toset([var.azs[0]]) : toset(var.azs)

  allocation_id = aws_eip.nat[each.key].id
  subnet_id     = aws_subnet.public[each.key].id

  tags = merge(local.tags, { Name = "${var.name}-nat-${each.key}" })

  depends_on = [aws_internet_gateway.this]
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.this.id
  tags   = merge(local.tags, { Name = "${var.name}-public" })

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.this.id
  }
}

resource "aws_route_table_association" "public" {
  for_each       = aws_subnet.public
  subnet_id      = each.value.id
  route_table_id = aws_route_table.public.id
}

# One private route table per AZ when NAT is per-AZ (so an AZ's egress
# never crosses to another AZ's NAT); one shared table when single_nat_gateway.
resource "aws_route_table" "private" {
  for_each = var.single_nat_gateway ? toset(["shared"]) : toset(var.azs)

  vpc_id = aws_vpc.this.id
  tags   = merge(local.tags, { Name = "${var.name}-private-${each.key}" })

  route {
    cidr_block     = "0.0.0.0/0"
    nat_gateway_id = var.single_nat_gateway ? aws_nat_gateway.this[var.azs[0]].id : aws_nat_gateway.this[each.key].id
  }
}

resource "aws_route_table_association" "private" {
  for_each       = aws_subnet.private
  subnet_id      = each.value.id
  route_table_id = var.single_nat_gateway ? aws_route_table.private["shared"].id : aws_route_table.private[each.key].id
}
