output "cluster_name" {
  description = "EKS cluster name."
  value       = module.eks.cluster_name
}

output "cluster_endpoint" {
  description = "EKS API server endpoint."
  value       = module.eks.cluster_endpoint
}

output "region" {
  description = "AWS region the cluster runs in."
  value       = var.region
}

# The cluster's CA bundle, because infra/aws/argo-bootstrap asks for it by name.
#
# Its README says to pass `cluster_ca=<from cluster output>` and there was no such output, so the documented
# procedure could not be followed as written -- the value had to be fetched with a separate
# `aws eks describe-cluster --query cluster.certificateAuthority.data` that the README never mentions.
#
# A runbook step that cannot be executed as written is the same defect as a guard that cannot fire: it reads
# like coverage until someone tries it.
output "cluster_ca" {
  description = "Base64 CA bundle for the cluster's API server, for the Kubernetes and Helm providers in infra/aws/argo-bootstrap."
  value       = module.eks.cluster_certificate_authority_data
}
