# Where each GPU node group can actually run, rather than wherever subnet zero happens to be.
#
# Every node group pinned to module.vpc.public_subnets[0], which is the first of the two AZs the VPC was
# built in. That worked by luck and would have kept working until it did not: in ap-northeast-2 the
# availability zones come back a, b, c, d, so subnet zero is 2a, and g5 is offered in 2a, 2c and 2d --
# but NOT in 2b. Lose 2a from the available list for any reason and the slice becomes [2b, 2c], subnet zero
# becomes 2b, and the A10G groups have nowhere to run.
#
# The failure would not have surfaced at apply. Every GPU group runs at desired_size 0, so terraform builds
# the group and launches nothing; the InsufficientInstanceCapacity or unsupported-type error arrives when a
# session scales up, on a cluster that has looked healthy for hours. This is the same shape as the region
# defaulting to us-east-1, and it is worth solving the same way: derive the placement instead of assuming it.

data "aws_ec2_instance_type_offerings" "gpu" {
  location_type = "availability-zone"
  filter {
    name   = "instance-type"
    values = [var.gpu_node_instance_type]
  }
}

data "aws_ec2_instance_type_offerings" "gpu_single" {
  location_type = "availability-zone"
  filter {
    name   = "instance-type"
    values = [var.gpu_single_node_instance_type]
  }
}

data "aws_ec2_instance_type_offerings" "gpu_shared" {
  location_type = "availability-zone"
  filter {
    name   = "instance-type"
    values = [var.gpu_shared_node_instance_type]
  }
}

locals {
  # The VPC module builds one private subnet per AZ, in order, so this pairs them. Node groups live in the
  # private tier now; the public subnets hold the NAT gateway and no instances.
  az_subnet = zipmap(local.azs, module.vpc.private_subnets)

  # Subnets whose zone actually offers the type. Ordered by local.azs so the choice is deterministic
  # rather than dependent on the order AWS happened to return offerings in.
  gpu_subnets        = [for az in local.azs : local.az_subnet[az] if contains(data.aws_ec2_instance_type_offerings.gpu.locations, az)]
  gpu_single_subnets = [for az in local.azs : local.az_subnet[az] if contains(data.aws_ec2_instance_type_offerings.gpu_single.locations, az)]
  gpu_shared_subnets = [for az in local.azs : local.az_subnet[az] if contains(data.aws_ec2_instance_type_offerings.gpu_shared.locations, az)]
}

# A hard stop at plan time, because the alternative is a soft stop at session time.
#
# terraform_data carries no infrastructure; it exists so these preconditions have somewhere to live. A
# `check` block would only warn, and a warning is exactly what nobody reads before scaling a node group up.
resource "terraform_data" "gpu_placement" {
  input = {
    gpu        = join(",", local.gpu_subnets)
    gpu_single = join(",", local.gpu_single_subnets)
    gpu_shared = join(",", local.gpu_shared_subnets)
  }

  lifecycle {
    precondition {
      condition     = length(local.gpu_subnets) > 0
      error_message = "No subnet in this VPC is in a zone that offers ${var.gpu_node_instance_type}. queuelab needs two devices on one node and this is the only family size that provides them; either widen local.azs or move the region."
    }
    precondition {
      condition     = length(local.gpu_single_subnets) > 0
      error_message = "No subnet in this VPC is in a zone that offers ${var.gpu_single_node_instance_type}. M5-b's sizing in hack/m5b-vllm-sizing.md is derived for that card and does not transfer to another."
    }
    precondition {
      condition     = length(local.gpu_shared_subnets) > 0
      error_message = "No subnet in this VPC is in a zone that offers ${var.gpu_shared_node_instance_type}. M5-c puts two engines on one card and internal/bench.SharingPlan refuses that plan on anything smaller."
    }
  }
}

# Quota is a second axis, and nothing checked it.
#
# The offerings data above answers "does this zone sell this instance type". It does not answer "is this
# account allowed to run one", and those are different questions with the same symptom: a node group that
# creates cleanly at desired_size = 0 and fails at the first scale-up of a paid session.
#
# The specific miss this exists to prevent: on 2026-08-26 the account was granted 52 vCPU of
# "Running On-Demand G and VT instances" (L-DB2E81BA), and that was read as permission to run G instances.
# AWS meters Spot under a SEPARATE quota -- "All G and VT Spot Instance Requests" (L-3819A6DF), which is 0
# here and was never requested. Two node groups were switched to SPOT on that misreading, and nothing in
# terraform would have objected: capacity_type is not validated against a quota, and desired_size = 0 means
# no instance is ever launched at apply.
#
# aws_servicequotas_service_quota is a data source rather than a resource on purpose. Managing the quota
# would make terraform try to raise it, which is a support case with a human on the other end, not a
# resource. This only reads it and refuses.
data "aws_servicequotas_service_quota" "gpu_on_demand" {
  quota_code   = "L-DB2E81BA"
  service_code = "ec2"
}

data "aws_servicequotas_service_quota" "gpu_spot" {
  quota_code   = "L-3819A6DF"
  service_code = "ec2"
}

# vCPU counts come from EC2, not from a table in this file.
#
# They were a hand-written map with a lookup default: 48 for the multi-GPU type and 4 for the single-card
# ones. That default is a fail-open on exactly the change the guard exists to catch. Point
# gpu_single_node_instance_type at g6.12xlarge -- 48 vCPU -- and the lookup returns 4, the precondition
# compares 4 against the quota, the plan passes, and the failure waits for the scale-up of a paid session.
#
# The old comment defended the map by saying a describe call "can fail for reasons unrelated to what is being
# checked". It can, and a failed data source stops the plan loudly, which is the correct outcome. A silent
# wrong answer is not.
data "aws_ec2_instance_type" "gpu" {
  instance_type = var.gpu_node_instance_type
}

data "aws_ec2_instance_type" "gpu_single" {
  instance_type = var.gpu_single_node_instance_type
}

data "aws_ec2_instance_type" "gpu_shared" {
  instance_type = var.gpu_shared_node_instance_type
}

locals {
  # The largest vCPU demand any single group can reach, which is its max_size times its instance -- not one
  # instance.
  #
  # This was a max() over the three instance types, giving 48, and 52 vCPU passed it. That hid the case the
  # study actually promises: queuelab's preregistration includes a TWO-NODE axis, so a two-node run asks for
  # 2 x 48 = 96 vCPU against a granted 52. The node group would scale to one node, stall, and the second
  # would never arrive -- during a session, on a machine billing $4.812/hour.
  #
  # Groups run one at a time, so this is a max across groups rather than a sum. The two "* 1" factors mirror
  # the single-card groups' max_size in eks.tf and are the only hand-carried numbers left in this file.
  gpu_group_vcpus = {
    gpu        = data.aws_ec2_instance_type.gpu.default_vcpus * var.gpu_max_nodes
    gpu_single = data.aws_ec2_instance_type.gpu_single.default_vcpus * 1
    gpu_shared = data.aws_ec2_instance_type.gpu_shared.default_vcpus * 1
  }


  # Demand is per PURCHASING OPTION, because the quotas are.
  #
  # This took a single max across every group and compared it to whichever quota was in play. With queuelab
  # on On-Demand at 48 vCPU and only the 4-vCPU M5-b group on Spot, that compared 48 against the 8-vCPU Spot
  # quota and refused a configuration that fits comfortably. The mirror of that error is worse: a coarse
  # maximum can also pass a group whose own quota cannot hold it.
  #
  # Groups run one at a time within an option, so each side is a max rather than a sum.
  gpu_on_demand_vcpus = [for k, v in local.gpu_group_vcpus : v if local.gpu_capacity[k] == "ON_DEMAND"]
  gpu_spot_vcpus      = [for k, v in local.gpu_group_vcpus : v if local.gpu_capacity[k] == "SPOT"]

  gpu_on_demand_needed = length(local.gpu_on_demand_vcpus) > 0 ? max(local.gpu_on_demand_vcpus...) : 0
  gpu_spot_needed      = length(local.gpu_spot_vcpus) > 0 ? max(local.gpu_spot_vcpus...) : 0

  gpu_on_demand_hungriest = join(",", [for k, v in local.gpu_group_vcpus : k if local.gpu_capacity[k] == "ON_DEMAND" && v == local.gpu_on_demand_needed])
  gpu_spot_hungriest      = join(",", [for k, v in local.gpu_group_vcpus : k if local.gpu_capacity[k] == "SPOT" && v == local.gpu_spot_needed])
}

resource "terraform_data" "gpu_quota" {
  input = {
    on_demand_quota  = data.aws_servicequotas_service_quota.gpu_on_demand.value
    spot_quota       = data.aws_servicequotas_service_quota.gpu_spot.value
    on_demand_needed = local.gpu_on_demand_needed
    spot_needed      = local.gpu_spot_needed
  }

  lifecycle {
    precondition {
      condition     = local.gpu_on_demand_needed == 0 || data.aws_servicequotas_service_quota.gpu_on_demand.value >= local.gpu_on_demand_needed
      error_message = "Running On-Demand G and VT instances (L-DB2E81BA) is ${data.aws_servicequotas_service_quota.gpu_on_demand.value} vCPU. On-Demand node group(s) '${local.gpu_on_demand_hungriest}' can reach ${local.gpu_on_demand_needed} vCPU at max_size. They would create at desired_size 0 and stall partway through a scale-up during a paid session. Request the increase, or lower that group's max_size."
    }
    precondition {
      condition     = local.gpu_spot_needed == 0 || data.aws_servicequotas_service_quota.gpu_spot.value >= local.gpu_spot_needed
      error_message = "All G and VT Spot Instance Requests (L-3819A6DF) is ${data.aws_servicequotas_service_quota.gpu_spot.value} vCPU. Spot node group(s) '${local.gpu_spot_hungriest}' can reach ${local.gpu_spot_needed} vCPU at max_size. The On-Demand increase does NOT raise this -- it is a separate quota. Either request it or set capacity_type back to ON_DEMAND."
    }
  }
}
