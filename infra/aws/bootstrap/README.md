# infra/aws/bootstrap

Creates the Terraform state backend (S3, locking on a state-adjacent object), the GitHub OIDC provider,
the CI IAM roles, and the ECR repository. This root is the chicken-and-egg base:
it begins on local state, then migrates into the bucket it just created.

Nothing here is provisioned yet. This is the documented procedure for when it is.

## One-time bootstrap

```bash
cd infra/aws/bootstrap

# 1. Create the backend on local state.
terraform init
terraform apply \
  -var 'state_bucket_name=<globally-unique-name>' \
  -var 'github_repo=<owner>/<name>' \
  -var "github_repository_id=$(gh api repos/<owner>/<name> --jq .id)" \
  -var "github_repository_owner_id=$(gh api repos/<owner>/<name> --jq .owner.id)" \
  -var 'budget_notification_emails=["you@example.com"]'

# ALWAYS pass budget_notification_emails, on every apply and not only the first.
#
# The budget's notifications are dynamic blocks gated on that list, and it defaults to empty. Omitting it
# does not leave the existing alerts alone: Terraform sees a budget with no notifications configured and
# deletes the ones the account has. The command above without that last line removes every cost alert on
# this account as a side effect of whatever else the apply was for.
#
# This nearly happened on 2026-09-04. A plan for three unrelated ECR repositories showed all three
# notifications queued for deletion, including the 10 percent one that had, the day before, delivered the
# alert revealing the month at 14.56 dollars against a running estimate of about 8.

# 2. Migrate the local state into the bucket just created.
#
# Done for this account on 2026-08-29. backend.tf is committed; bucket and kms_key_id are deliberately
# absent from it, because they are account coordinates and this repository is public. Supply them here.
terraform init -migrate-state \
  -backend-config="bucket=$(terraform output -raw state_bucket)" \
  -backend-config="kms_key_id=$(terraform output -raw state_kms_key_arn)"

# 3. Remove the emptied local state. terraform leaves terraform.tfstate.backup; keep it until the S3 state
#    has been read by a real plan, then delete it. A stale local state is the one way this migration can be
#    undone by accident.
rm -f terraform.tfstate
```

After migration, `bootstrap` is applied only to rotate identity or the registry,
never on routine cluster work.

## Required GitHub Environment protection (one-time, manual)

The apply role's OIDC trust condition pins the sub to
`repo:<owner/name>:environment:infra-apply`. That condition denies applies from
forked PRs and non-main branches only when the GitHub Environment enforces it.
In the repository settings, create an Environment named `infra-apply` with:

- a deployment branch policy restricting deployments to the `main` branch, and
- at least one required reviewer.

Without this Environment protection, the sub condition alone does not prevent a
workflow edited in a fork from assuming the apply role.

## Unattended teardown note

`destroy.yml` runs on a nightly schedule but uses the `infra-apply` environment
so its OIDC token can assume the apply role. If `infra-apply` requires a
reviewer, the scheduled teardown will pause for manual approval. For fully
unattended teardown, provision a separate `infra-destroy` environment and a
dedicated destroy role whose trust pins `sub` to that environment, without a
reviewer gate.

## Required repository secrets and variables (one-time, manual)

After `terraform apply` of this bootstrap root, set these in the GitHub repo so
the workflows can assume the roles and complete the cluster backend:

Secrets:
- `CI_PLAN_ROLE_ARN` from output `ci_plan_role_arn`
- `CI_APPLY_ROLE_ARN` from output `ci_apply_role_arn`
- `CI_IMAGE_PUSH_ROLE_ARN` from output `ci_image_push_role_arn`

Variables:
- `TF_STATE_BUCKET` from output `state_bucket`
- `TF_STATE_KMS_KEY` from output `state_kms_key_arn`
