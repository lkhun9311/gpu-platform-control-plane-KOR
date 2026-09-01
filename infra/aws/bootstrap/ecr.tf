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
