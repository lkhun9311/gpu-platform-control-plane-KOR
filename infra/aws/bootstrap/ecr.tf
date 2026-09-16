# The operator image registry.
#
# The lifecycle policy expires only untagged images, never a tag referenced from the default branch.
resource "aws_ecr_repository" "operator" {
  name                 = var.operator_repo_name
  image_tag_mutability = "IMMUTABLE"
  tags                 = var.tags

  image_scanning_configuration {
    scan_on_push = true
  }

  encryption_configuration {
    encryption_type = "KMS"
  }
}

resource "aws_ecr_lifecycle_policy" "operator" {
  repository = aws_ecr_repository.operator.name

  policy = jsonencode({
    rules = [{
      rulePriority = 1
      description  = "Expire untagged images after 14 days."
      selection = {
        tagStatus   = "untagged"
        countType   = "sinceImagePushed"
        countUnit   = "days"
        countNumber = 14
      }
      action = {
        type = "expire"
      }
    }]
  })
}

# The gateway image registry.
#
# There was none, and config/gateway/deployment.yaml carried `image: gateway:latest` -- a name nothing
# publishes. Argo CD would deploy it and the Pod would sit ImagePullBackOff, while the paid session scripts
# built their own copy under a local tag and pushed it wherever REGISTRY happened to point.
#
# Same settings as the operator's for the same reasons: IMMUTABLE tags so a digest cannot be re-pointed under
# a name someone already recorded, scan on push, KMS at rest, and untagged expiry.
resource "aws_ecr_repository" "gateway" {
  name                 = var.gateway_repo_name
  image_tag_mutability = "IMMUTABLE"
  tags                 = var.tags

  image_scanning_configuration {
    scan_on_push = true
  }

  encryption_configuration {
    encryption_type = "KMS"
  }
}

resource "aws_ecr_lifecycle_policy" "gateway" {
  repository = aws_ecr_repository.gateway.name

  policy = jsonencode({
    rules = [{
      rulePriority = 1
      description  = "Expire untagged images after 14 days."
      selection = {
        tagStatus   = "untagged"
        countType   = "sinceImagePushed"
        countUnit   = "days"
        countNumber = 14
      }
      action = {
        type = "expire"
      }
    }]
  })
}

# Mirrors of the public images a fresh GPU node pulls.
#
# Every byte of those pulls crosses the NAT gateway at $0.059/GB, because the engine image comes from Docker
# Hub and the two NVIDIA components from nvcr.io -- neither reachable through the free S3 gateway endpoint
# this VPC already has. docs/09 worked that out on 2026-08-27, said mirroring into ECR was the change that
# would move the traffic onto the endpoint, and recorded that it had not been done. The bill for the first
# three paid sessions arrived on 2026-09-03: $5.43 of NAT data processing, about 92 GB, the single largest
# line and larger than every instance-hour combined.
#
# ECR layers are served from S3, so a pull from here goes over the gateway endpoint at no charge. The ECR
# API calls that precede it -- auth and manifest, kilobytes -- still cross the NAT, which is why no interface
# endpoint is added: at that volume one would cost more in ENI-hours than it saves.
#
# for_each rather than three copies of the block above, because a value repeated per-resource is how the
# operator and gateway repositories drifted into having different settings the first time one was edited.
resource "aws_ecr_repository" "mirror" {
  for_each = var.mirrored_images

  name = each.key
  # Digests are what the manifests pin and what the mirror script verifies, so a mutable tag would only
  # create a second name for the same content that nothing checks.
  image_tag_mutability = "IMMUTABLE"
  tags                 = var.tags

  image_scanning_configuration {
    scan_on_push = true
  }

  encryption_configuration {
    encryption_type = "KMS"
  }
}

# No lifecycle policy on the mirrors, deliberately.
#
# The operator and gateway repositories expire untagged images because CI pushes a new one on every merge.
# A mirror holds a handful of upstream releases pinned by digest from the manifests, and expiring one would
# break a cluster build for a version the tree still references. They are deleted by hand when the manifest
# stops naming them.
