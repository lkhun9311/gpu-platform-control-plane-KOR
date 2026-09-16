# The state for this root lives in the bucket this root creates.
#
# That is the chicken-and-egg the README describes, and it is resolved by sequence rather than by cleverness:
# bootstrap runs once on local state to create the bucket, then migrates into it with
# `terraform init -migrate-state`. This file is the second half of that, and it was never applied until
# 2026-08-29 -- the state sat on one laptop, gitignored, with the bucket holding zero objects. Losing that
# file would have orphaned every bootstrap resource: the bucket, the KMS key, the OIDC provider, three IAM
# roles, ECR and the budget, recoverable only by hand or by `terraform import`.
#
# The consequence to know before destroying anything: `terraform destroy` on this root would try to delete
# the bucket holding its own state. bootstrap is not a routine destroy target, but if it ever has to go, the
# state must be migrated back to local first.
#
# bucket and kms_key_id are deliberately absent. They are account coordinates, this repository is public, and
# the cluster and argo-bootstrap roots already take them at init time:
#
#   terraform init -migrate-state \
#     -backend-config="bucket=$(terraform output -raw state_bucket)" \
#     -backend-config="kms_key_id=$(terraform output -raw state_kms_key_arn)"
terraform {
  backend "s3" {
    key     = "bootstrap/terraform.tfstate"
    region  = "ap-northeast-2"
    encrypt = true

    # The lock is an object beside the state at <key>.tflock, written with If-None-Match. See the note in
    # infra/aws/cluster/backend.tf for why there is no DynamoDB table.
    use_lockfile = true
  }
}
