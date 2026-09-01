variable "region" {
  description = "AWS region for the state backend and registry."
  type        = string
  # ap-northeast-2, because that is where the GPU quota lives.
  #
  # This defaulted to us-east-1 while the account's "Running On-Demand G and VT instances" quota was raised
  # to 52 vCPU in Seoul on 2026-08-26. The mismatch would not have failed the apply: every GPU node group
  # runs at desired_size 0, so no G instance is launched until a session scales one up -- and the quota error
  # would have arrived there, on a cluster that had looked fine for hours.
  #
  # Changing it means the state bucket, the cluster and the registry all live beside the quota. A region set
  # in one place and not the others is the same failure with an extra step.
  default = "ap-northeast-2"
}

variable "state_bucket_name" {
  description = "Globally unique S3 bucket name for Terraform state."
  type        = string
}

variable "operator_repo_name" {
  description = "ECR repository name for the operator image."
  type        = string
  default     = "gpu-platform-operator"
}

variable "state_key_user_arns" {
  description = "IAM role ARNs allowed to use the state KMS key for encrypt and decrypt."
  type        = list(string)
  default     = []
}

variable "tags" {
  description = "Owner and TTL tags applied to every resource."
  type        = map(string)
  default = {
    project = "gpu-platform-control-plane"
    owner   = "lkhun9311"
    ttl     = "ephemeral"
  }
}

variable "github_repo" {
  description = "owner/name of the GitHub repository allowed to assume the CI roles."
  type        = string
}

# job_workflow_ref is not an AWS IAM trust condition key.
#
# Workflow identity, if it must be pinned, is encoded through GitHub's customized sub template, not here.
#
# Immutable owner/repo-ID sub claims require opt-in for repos created before 2026-07-15 (this one).
#
# This toggle stays off until that opt-in is enabled.
variable "use_immutable_sub" {
  description = "Whether the OIDC sub uses immutable owner/repo IDs (requires GitHub opt-in)."
  type        = bool
  default     = false
}

variable "gateway_repo_name" {
  description = "ECR repository name for the gateway image."
  type        = string
  default     = "gpu-platform-gateway"
}

variable "github_repository_id" {
  description = "Numeric GitHub repository ID, used as an immutable OIDC trust condition."
  type        = string

  # Numeric rather than the owner/name string, because names move.
  #
  # `gh api repos/<owner>/<name> --jq .id` prints it. It has no default: a wrong value here silently narrows
  # the trust to nothing, and a default would let that pass review as "the value that was already there".
}

variable "github_repository_owner_id" {
  description = "Numeric GitHub account ID of the repository owner, used as an immutable OIDC trust condition."
  type        = string

  # `gh api repos/<owner>/<name> --jq .owner.id`.
}
