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
