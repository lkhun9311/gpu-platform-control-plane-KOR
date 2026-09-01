# The CI apply role is resolved from its name rather than passed in as an ARN.
#
# See the comment on var.ci_apply_role_name: the ARN form was never supplied by any caller, so the access
# entry below counted to zero on every plan ever produced here. A lookup cannot be forgotten.
data "aws_iam_role" "ci_apply" {
  count = var.ci_apply_role_name == "" ? 0 : 1
  name  = var.ci_apply_role_name
}

# The CI apply role needs kubectl to run pre-destroy teardown in a later increment.
#
# Without this access entry, destroy.yml cannot delete Argo apps in order.
#
# The entry is created only when the role ARN is supplied, so validate and plan work before bootstrap exists.
resource "aws_eks_access_entry" "ci_apply" {
  count         = var.ci_apply_role_name == "" ? 0 : 1
  cluster_name  = module.eks.cluster_name
  principal_arn = data.aws_iam_role.ci_apply[0].arn
  type          = "STANDARD"
}

resource "aws_eks_access_policy_association" "ci_apply_admin" {
  count         = var.ci_apply_role_name == "" ? 0 : 1
  cluster_name  = module.eks.cluster_name
  principal_arn = data.aws_iam_role.ci_apply[0].arn
  policy_arn    = "arn:aws:eks::aws:cluster-access-policy/AmazonEKSClusterAdminPolicy"

  access_scope {
    type = "cluster"
  }

  # AWS requires the access entry to exist before a policy association references its principal.
  #
  # The two resources share no attribute, so this explicit dependency is what orders them.
  depends_on = [aws_eks_access_entry.ci_apply]
}
