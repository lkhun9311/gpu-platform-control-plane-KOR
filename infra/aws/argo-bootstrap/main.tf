# This root installs Argo CD once and owns nothing else.
#
# It is never run on routine cluster plan or apply.
#
# After install, Argo CD owns in-cluster resources and this release is left alone.
resource "helm_release" "argocd" {
  name             = "argo-cd"
  namespace        = var.argocd_namespace
  create_namespace = true

  repository = "https://argoproj.github.io/argo-helm"
  chart      = "argo-cd"
  version    = var.argocd_chart_version

  # The chart's cluster-scoped CRDs are installed here once.
  #
  # Routine applies never own them, matching the ownership boundary in docs/09.
  skip_crds = false
}
