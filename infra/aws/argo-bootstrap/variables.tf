variable "region" {
  description = "AWS region."
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

variable "cluster_name" {
  description = "EKS cluster name (bootstrap output from the cluster root)."
  type        = string
  default     = "gpu-platform"
}

variable "cluster_endpoint" {
  description = "EKS API server endpoint."
  type        = string
}

variable "cluster_ca" {
  description = "Base64-encoded cluster CA certificate."
  type        = string
}

variable "argocd_chart_version" {
  description = "Pinned argo-cd Helm chart version from the argo-helm repository."
  type        = string
  default     = "7.7.0"
}

variable "argocd_namespace" {
  description = "Namespace Argo CD is installed into."
  type        = string
  default     = "argocd"
}
