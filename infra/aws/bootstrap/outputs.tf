output "state_bucket" {
  description = "S3 bucket name for Terraform state."
  value       = aws_s3_bucket.state.id
}

output "state_kms_key_arn" {
  description = "KMS key ARN encrypting the state bucket."
  value       = aws_kms_key.state.arn
}

output "operator_repo_url" {
  description = "ECR repository URL for the operator image."
  value       = aws_ecr_repository.operator.repository_url
}

output "ci_plan_role_arn" {
  description = "IAM role ARN for terraform plan (PR context)."
  value       = aws_iam_role.ci_plan.arn
}

output "ci_apply_role_arn" {
  description = "IAM role ARN for terraform apply (protected environment)."
  value       = aws_iam_role.ci_apply.arn
}

output "ci_image_push_role_arn" {
  description = "IAM role ARN for pushing the operator image to ECR."
  value       = aws_iam_role.ci_image_push.arn
}

output "gateway_repo_url" {
  description = "ECR repository URL for the gateway image."
  value       = aws_ecr_repository.gateway.repository_url
}

# The role EventBridge Scheduler assumes to force a GPU node group to zero when a session's TTL expires.
#
# The harness reads this to arm a schedule before it scales anything up, and refuses to start the GPU if the
# schedule cannot be created -- so the deadline exists before the cost does.
output "ttl_scaledown_role_arn" {
  description = "IAM role assumed by EventBridge Scheduler for the session TTL scale-down."
  value       = aws_iam_role.ttl_scaledown.arn
}

output "mirror_repository_urls" {
  description = "Mirror repository URLs, keyed by repository name."
  value       = { for k, r in aws_ecr_repository.mirror : k => r.repository_url }
}
