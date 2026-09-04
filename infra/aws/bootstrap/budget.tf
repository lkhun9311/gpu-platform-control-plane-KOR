variable "budget_limit_usd" {
  description = "Monthly cost budget ceiling for the demo account in USD."
  type        = string
  default     = "30"
}

variable "budget_notification_emails" {
  description = "Email addresses notified when the budget threshold is crossed."
  type        = list(string)
  default     = []
}

# A monthly cost budget backs the destroy workflow's budget-alarm escalation.
#
# The design's ceiling is a hard review at 30 USD cumulative, so the default limit is 30.
#
# Notifications are created only when at least one email is supplied, so validate and apply work with the empty default.
resource "aws_budgets_budget" "demo" {
  name         = "gpu-platform-demo-monthly"
  budget_type  = "COST"
  limit_amount = var.budget_limit_usd
  limit_unit   = "USD"
  time_unit    = "MONTHLY"

  # The early warning, at 10 percent of the budget.
  #
  # It exists because 80 percent of a monthly budget is not an early warning for a project whose spend
  # arrives in three-hour bursts: the 2026-09-03 session was over before a month-scale threshold would have
  # said anything. At a 30 dollar budget this fires at 3 dollars, which is roughly one paid session, so the
  # first session of a month reports what it cost while the next one can still be reconsidered.
  #
  # It was created by hand on 2026-08-31 and never written down here. On 2026-09-03 it delivered the alert
  # that showed the month at 14.56 dollars against my own running total of about 8 -- the alert that found
  # the NAT data-processing charge my estimates had been omitting entirely.
  #
  # Terraform would have deleted it. A plan run before adding the mirror repositories showed this block
  # missing from the configuration and present in the account, so the apply would have removed the only
  # thing that has ever caught a cost error here, silently, as a side effect of an unrelated change. That is
  # the shape this project keeps producing: a value living in one place and not the other, with nothing
  # comparing them.
  dynamic "notification" {
    for_each = length(var.budget_notification_emails) > 0 ? [1] : []

    content {
      comparison_operator        = "GREATER_THAN"
      threshold                  = 10
      threshold_type             = "PERCENTAGE"
      notification_type          = "ACTUAL"
      subscriber_email_addresses = var.budget_notification_emails
    }
  }

  dynamic "notification" {
    for_each = length(var.budget_notification_emails) > 0 ? [1] : []

    content {
      comparison_operator        = "GREATER_THAN"
      threshold                  = 80
      threshold_type             = "PERCENTAGE"
      notification_type          = "ACTUAL"
      subscriber_email_addresses = var.budget_notification_emails
    }
  }

  dynamic "notification" {
    for_each = length(var.budget_notification_emails) > 0 ? [1] : []

    content {
      comparison_operator        = "GREATER_THAN"
      threshold                  = 100
      threshold_type             = "PERCENTAGE"
      notification_type          = "FORECASTED"
      subscriber_email_addresses = var.budget_notification_emails
    }
  }
}
