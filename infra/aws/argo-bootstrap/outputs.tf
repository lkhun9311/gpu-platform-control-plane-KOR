output "argocd_namespace" {
  description = "Namespace Argo CD was installed into."
  value       = helm_release.argocd.namespace
}

output "argocd_chart_version" {
  description = "Installed argo-cd chart version."
  value       = helm_release.argocd.version
}
