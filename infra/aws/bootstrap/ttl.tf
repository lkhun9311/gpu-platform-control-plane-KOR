# The control that survives the laptop dying.
#
# Every teardown path in this repository runs in the operator's shell: a trap on EXIT/INT/TERM, or a person
# reading a banner. None of them run after the machine sleeps, loses power, or has its shell SIGKILLed, and
# none of them run once the SSO session expires -- which matters most because the scale-down is itself an AWS
# call, so an expired session breaks the exact request that stops the billing.
#
# hack/m5b-gpu-session.sh:57 says so in as many words: "an external TTL watchdog is the control this
# repository still does not have." This is that control.
#
# It lives in bootstrap rather than cluster on purpose. The cluster is created and destroyed around every
# session; the thing that stops a forgotten GPU has to outlive it, and an IAM role costs nothing to keep.
#
# EventBridge Scheduler calls the EKS API directly through a universal target, so there is no Lambda: no
# function to package, no runtime to keep current, and no second place for the teardown logic to drift from
# the first. The schedule itself is created per session by the harness, not here, because its deadline is a
# property of the run.
data "aws_iam_policy_document" "ttl_scaledown_assume" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["scheduler.amazonaws.com"]
    }

    # Confused-deputy guard. Without it, any AWS account able to name this role ARN in its own schedule could
    # borrow it, because the service principal alone does not say WHOSE schedule is calling.
    condition {
      test     = "StringEquals"
      variable = "aws:SourceAccount"
      values   = [data.aws_caller_identity.current.account_id]
    }
  }
}

data "aws_iam_policy_document" "ttl_scaledown" {
  statement {
    effect = "Allow"

    # One action, and deliberately not DeleteNodegroup.
    #
    # Scaling to zero stops the instance charge and leaves the node group definition in place, so terraform
    # still owns it and the next session does not have to recreate it. A watchdog that deleted the group
    # would put the account back in a state terraform did not describe, which is a worse failure than the one
    # it prevents.
    actions   = ["eks:UpdateNodegroupConfig"]
    resources = ["arn:aws:eks:*:${data.aws_caller_identity.current.account_id}:nodegroup/*/*/*"]
  }
}

resource "aws_iam_role" "ttl_scaledown" {
  name               = "gpu-platform-ttl-scaledown"
  description        = "Assumed by EventBridge Scheduler to force a GPU node group to desiredSize=0 when a session's TTL expires."
  assume_role_policy = data.aws_iam_policy_document.ttl_scaledown_assume.json
  tags               = var.tags
}

resource "aws_iam_role_policy" "ttl_scaledown" {
  name   = "ttl-scaledown"
  role   = aws_iam_role.ttl_scaledown.id
  policy = data.aws_iam_policy_document.ttl_scaledown.json
}
